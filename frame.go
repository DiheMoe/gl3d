package gl3d

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
)

// ErrSmallBuffer 表示输出缓冲不足以容纳一个采样帧。
var ErrSmallBuffer = errors.New("gl3d: output buffer smaller than one sample frame")

// decoderDelay 是 IMDCT 与合成滤波器组的结构延迟(528+1 个采样帧);
// gapless 裁剪的标签值须计入它:头裁 enc_delay+529,尾裁 enc_padding−529。
const decoderDelay = 529

// decodeFrame 取分帧层交出的下一个音频帧并解码;音频正常结束时调用 finish 做流结束
// 时的校验。
func (d *Decoder) decodeFrame(ctx context.Context) error {
	af, err := d.fr.next(ctx)
	if err == errEndOfAudio {
		return d.finish()
	}
	if err != nil {
		return err
	}
	if af.gap {
		d.rsv.reset()
	}
	if err := d.decodeAudio(af.data, af.header, !d.fr.strict && !af.unconfirmed); err != nil {
		if af.unconfirmed {
			// 源出错时未经确认交出的首帧解码失败,多半是伪同步:报告真正的原因
			return d.fr.s.err
		}
		return err
	}
	return nil
}

// decodeAudio 解码一个完整音频帧,PCM 追加到内部队列。conceal 为真时(宽松模式下
// 确认过的帧)帧内损坏记入报告后容错:CRC 不符照常解码;side info 非法或所需位数超出
// main data 时丢弃这一帧,它的 main data 仍并入 bit reservoir,供后续帧回指;
// scalefactor 或谱数据出错的 granule 按 decodeSpectra 留下的频谱继续。
func (d *Decoder) decodeAudio(frame []byte, fh frameHeader, conceal bool) error {
	rep := &d.fr.rep
	off := 4
	if fh.hasCRC {
		// CRC 覆盖帧头第 16–31 位与整个 side information
		stored := binary.BigEndian.Uint16(frame[4:6])
		if crc16Update(crc16(frame[2:4]), frame[6:6+fh.sideInfoSize]) != stored {
			err := &CorruptError{Frame: d.framesDecoded, Detail: "CRC mismatch"}
			if !conceal {
				return err
			}
			d.note(err)
			rep.CRCErrors++
		}
		off = 6
	}
	si := &d.si
	mainData := frame[off+fh.sideInfoSize : fh.frameSize]
	ngr := 2
	if fh.version.lsf() {
		ngr = 1
	}
	nch := fh.channels()
	err := parseSideInfo(&fh, frame[off:off+fh.sideInfoSize], si)
	if err == nil {
		// 表 0 与缺失历史的静音 granule 不读位,解码时发现不了声明的位数超出实际
		// 数据,先按整帧核对。assemble 给出的视图恰为 main_data_begin 字节(缺失的
		// 历史按占位计)加本帧自带的 main 字节
		bits := 0
		for gr := 0; gr < ngr; gr++ {
			for ch := 0; ch < nch; ch++ {
				bits += si.gr[gr][ch].part2_3Length
			}
		}
		if bits > 8*(si.mainDataBegin+len(mainData)) {
			err = &CorruptError{Frame: -1, Detail: "part2_3_length exceeds available main data"}
		}
	}
	if err != nil {
		err = wrapFrameErr(err, d.framesDecoded)
		if !conceal {
			return err
		}
		d.note(err)
		rep.DroppedFrames++
		d.dropped++
		// 分帧层交出这一帧时已经计数;它没有解出,分帧层此后记的异常仍按已解码的帧编号
		d.fr.frames--
		d.rsv.advance(mainData)
		return nil
	}
	window, missing := d.rsv.assemble(si.mainDataBegin, mainData)
	br := &bitReader{data: window}

	n := fh.samplesPerFrame * nch
	out := d.pcmReserve(n)
	silentGr0 := false
	for gr := 0; gr < ngr; gr++ {
		// 码流从中途开始时,开头 granule 的数据起点可能早于流内第一个 main data
		// 字节,无从解码;MPEG-1 第二个 granule 置了 scfsi 时还要复用第一个 granule
		// 的 scalefactor,第一个缺数据它也缺。这类 granule 按全零频谱变换(按 granule
		// 置零同 FFmpeg):输出静音,时长不变
		silent := br.pos < 8*missing || gr == 1 && silentGr0 && si.scfsi != [2][4]int{}
		if gr == 0 {
			silentGr0 = silent
		}
		// block_type 与 mixed_block_flag 逐声道传输,仅 MS stereo 要求两声道一致
		var bm [2]*bandMap
		for ch := 0; ch < nch; ch++ {
			shape := si.gr[gr][ch].shape()
			bm[ch] = bandMapFor(fh.version, fh.srIdx, shape)
			gs := &d.spec[ch]
			if silent {
				clear(gs.xr[:])
				br.pos += si.gr[gr][ch].part2_3Length
				continue
			}
			intensityR := fh.channelMode == modeJointStereo && fh.modeExt&1 != 0 && ch == 1
			if err := decodeSpectra(&fh, si, gr, ch, br, bm[ch], shape, d.prevScf[ch][:], intensityR, gs); err != nil {
				err = wrapFrameErr(err, d.framesDecoded)
				if !conceal {
					return err
				}
				d.note(err)
				rep.ConcealedGranules++
			}
			if gr == 0 && !fh.version.lsf() {
				d.prevScf[ch] = gs.scf
			}
		}
		if nch == 2 && !silent {
			// intensity stereo 的位置与界限按右声道的 scalefactor band 解释
			r := &d.spec[1]
			right := r.scf[:bm[1].scfLen]
			if r.hasIst {
				right = r.ist[:bm[1].scfLen]
			}
			processStereo(&fh, &si.gr[gr][1], bm[1], right, r.transmitted, &d.spec[0].xr, &r.xr)
		}
		for ch := 0; ch < nch; ch++ {
			gi := &si.gr[gr][ch]
			bt := gi.blockType
			if !gi.windowSwitching {
				bt = 0
			}
			// mixed_block_flag 置位时最低 2 个子带用长窗,起始、短、结束块与所有采样率皆然
			nLongSub := 0
			if gi.windowSwitching && gi.mixedBlock {
				nLongSub = 2
			}
			d.chState[ch].imdctBlock(&d.spec[ch].xr, bt, nLongSub, bm[ch], &d.td)
			synthGranule(&d.synthSt[ch], &d.td, out[gr*576*nch+ch:], nch)
		}
	}
	d.rsv.advance(mainData)
	d.commit(out, nch) // 整帧处理完才计入队列
	d.framesDecoded++
	d.pcmTotal += int64(fh.samplesPerFrame)
	return nil
}

// commit 把刚解码的一帧计入 PCM 队列:out 是它的交错采样,解码序号从 pcmTotal 起;
// 落在 gapless 裁剪区间内的采样帧丢弃。
func (d *Decoder) commit(out []float64, nch int) {
	pos := d.pcmTotal
	// 尾裁区间按帧在流中的位置给出,换算到解码序号要减去解码时丢弃的帧。换算后不低于
	// 头裁区间,先删尾裁区间就不改变头裁区间内的下标
	shift := d.dropped * int64(d.header.samplesPerFrame)
	keep := cutFrames(out, pos, max(d.tailFrom-shift, d.headTrim), max(d.tailTo-shift, d.headTrim), nch)
	keep = cutFrames(keep, pos, 0, d.headTrim, nch)
	d.pcm = d.pcm[:len(d.pcm)+len(keep)]
}

// cutFrames 从交错采样 s(解码序号从 pos 起)中删去序号在 [from, to) 内的采样帧,
// 其后的采样前移,返回余下的部分。
func cutFrames(s []float64, pos, from, to int64, nch int) []float64 {
	n := int64(len(s) / nch)
	lo, hi := max(from-pos, 0), min(to-pos, n)
	if lo >= hi {
		return s
	}
	c := int64(nch)
	copy(s[lo*c:], s[hi*c:])
	return s[:(n-(hi-lo))*c]
}

// pcmReserve 确保 PCM 队列尾部有 n 个空位并返回该区间(尚未计入队列)。
// 已交付样本只在空间不足时整体搬走;ReadPCM 只在队列交付完时解码,未交付样本
// 至多一帧,队列容量因此有界。
func (d *Decoder) pcmReserve(n int) []float64 {
	if len(d.pcm)+n > cap(d.pcm) {
		live := copy(d.pcm, d.pcm[d.pcmOff:])
		d.pcm, d.pcmOff = d.pcm[:live], 0
		if live+n > cap(d.pcm) {
			grown := make([]float64, live, 2*(live+n))
			copy(grown, d.pcm)
			d.pcm = grown
		}
	}
	return d.pcm[len(d.pcm) : len(d.pcm)+n]
}

// finish 在流结束时核对帧数声明与 gapless 填充。
func (d *Decoder) finish() error {
	if n := d.declaredFrames(); n >= 0 && d.framesDecoded != n {
		what := "audio frame count vs VBRI declaration"
		if d.xing != nil {
			what = "audio frame count vs Xing declaration"
		}
		if err := d.tolerate(&TruncatedError{Offset: d.fr.offset(), What: what}); err != nil {
			return err
		}
	}
	if r := &d.fr.rep; !d.fr.strict && d.framesDecoded == 0 && (r.DroppedFrames > 0 || r.SkippedBytes > 0 || r.Truncated) {
		// 宽松模式丢掉了数据而一个帧也没有解出:没有可以交付的音频。没有帧被丢弃、跳过
		// 或截断就什么也没丢,流里本来就没有音频帧(只有标签帧),照常结束
		return ErrNoAudio
	}
	if d.badPadding {
		if err := d.tolerate(&CorruptError{Frame: -1, Detail: "gapless padding exceeds decoded length"}); err != nil {
			return err
		}
	}
	d.finished = true
	return nil
}

// tolerate 记下流结束时发现的异常:严格模式返回 err,宽松模式返回 nil 继续。
func (d *Decoder) tolerate(err error) error {
	d.note(err)
	if d.fr.strict {
		return err
	}
	return nil
}

// ReadPCM 向 buf 写入交错 float64 样本,返回交付的采样帧数。零长度 buf
// 不推进解码;len(buf) 不足一个采样帧返回 ErrSmallBuffer;正常非空请求
// 不会无限返回 (0, nil)。流结束返回 io.EOF,此前已交付的样本有效。
func (d *Decoder) ReadPCM(ctx context.Context, buf []float64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	nch := d.Channels()
	if len(buf) < nch {
		return 0, ErrSmallBuffer
	}
	for {
		avail := (len(d.pcm) - d.pcmOff) / nch
		if avail > 0 {
			n := min(avail, len(buf)/nch)
			copy(buf, d.pcm[d.pcmOff:d.pcmOff+n*nch])
			d.pcmOff += n * nch
			d.delivered += int64(n)
			return n, nil
		}
		if d.finished {
			if d.termErr != nil {
				return 0, d.termErr
			}
			return 0, io.EOF
		}
		// 解码出错:已缓冲样本照常交付,错误排空后返回
		if err := d.decodeFrame(ctx); err != nil {
			d.termErr = err
			d.finished = true
			d.note(err)
		}
	}
}

// DecodedFrames 返回原始解码的 PCM 采样帧数(不含 gapless 裁剪)。
func (d *Decoder) DecodedFrames() int64 { return d.pcmTotal }

// Frames 返回实际交付的 PCM 采样帧数(gapless 裁剪后);io.EOF 后为最终值。
func (d *Decoder) Frames() int64 { return d.delivered }

// Trimmed 报告是否应用了 gapless 裁剪;为 false 时 Frames 代表解码输出
// 长度,不代表编码前音频长度。尾裁需要帧数声明(Report().DeclaredFrames >= 0),
// 只有头裁时 Frames 仍不是编码器的输入长度。
func (d *Decoder) Trimmed() bool { return d.trimming }

// Report 返回到目前为止的解码报告。
func (d *Decoder) Report() Report {
	r := d.fr.rep
	r.DeclaredFrames, r.AudioFrames = d.declaredFrames(), d.framesDecoded
	if r.First != nil {
		first := *r.First
		r.First = &first
	}
	return r
}

// note 把解码器发现的异常记入报告:帧号为此前已解码的音频帧数,偏移为正在解码的帧的
// 起点;流结束时发现的异常偏移为流的结尾。错误自带的位置优先。
func (d *Decoder) note(err error) { d.fr.rep.note(err, d.framesDecoded, d.fr.offset()) }

// wrapFrameErr 给解码深处产生的 CorruptError 补上帧序号。
func wrapFrameErr(err error, frame int64) error {
	if c, ok := err.(*CorruptError); ok {
		c.Frame = frame
		return err
	}
	var c *CorruptError
	if errors.As(err, &c) {
		c.Frame = frame
	}
	return err
}
