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

// decodeFrame 解码一个音频帧,PCM 追加到内部队列;流结束时调用 finish 做
// 尾部校验与 gapless 尾裁。
func (d *Decoder) decodeFrame(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// 帧长以本帧帧头为准(VBR 逐帧可变),先读 4 字节帧头
	if err := d.s.fill(ctx, 4); err != nil {
		return err
	}
	if len(d.s.buf) == 0 {
		return d.finish()
	}
	if isID3v1Start(d.s.buf) {
		return d.skipID3v1(ctx)
	}
	if len(d.s.buf) < 4 {
		return d.trailingBytesErr(d.s.buf)
	}
	fh, err := parseFrameHeader([4]byte(d.s.buf[:4]))
	if err != nil {
		return wrapFrameErr(err, d.framesDecoded)
	}
	// 版本、采样率、声道数中途改变属范围外格式;码率与 padding 可逐帧变化
	if fh.version != d.header.version || fh.sampleRate != d.header.sampleRate || fh.channels() != d.header.channels() {
		return &UnsupportedError{Feature: "mid-stream format change"}
	}
	// 同步只在初始化时确认。当前完整帧不依赖下一帧是否已经到达或合法;
	// 后续头部/尾部的错误在下一轮报告,先交付已经收到的完整音频。
	readErr := d.s.fill(ctx, fh.frameSize)
	if len(d.s.buf) < fh.frameSize {
		if readErr != nil {
			return readErr
		}
		return &TruncatedError{Offset: d.s.tell(), What: "frame body"}
	}

	frame := d.s.buf[:fh.frameSize]
	off := 4
	if fh.hasCRC {
		// CRC 覆盖帧头第 16–31 位与整个 side information
		stored := binary.BigEndian.Uint16(frame[4:6])
		if crc16Update(crc16(frame[2:4]), frame[6:6+fh.sideInfoSize]) != stored {
			return &CorruptError{Frame: d.framesDecoded, Detail: "CRC mismatch"}
		}
		off = 6
	}
	si := &d.si
	if err := parseSideInfo(&fh, frame[off:off+fh.sideInfoSize], si); err != nil {
		return wrapFrameErr(err, d.framesDecoded)
	}
	mainData := frame[off+fh.sideInfoSize : fh.frameSize]
	window, missing := d.rsv.assemble(si.mainDataBegin, mainData)
	br := &bitReader{data: window}

	ngr := 2
	if fh.version.lsf() {
		ngr = 1
	}
	nch := fh.channels()
	// 表 0 与缺失历史的静音分支可能不读任何位,因此不能只靠读位操作
	// 检测物理截断。先验证整帧声明的预算,包括虚拟缺失历史的占位前缀。
	bitsNeeded := 0
	for gr := 0; gr < ngr; gr++ {
		for ch := 0; ch < nch; ch++ {
			bitsNeeded += si.gr[gr][ch].part2_3Length
		}
	}
	if bitsNeeded > len(window)*8 {
		return &CorruptError{Frame: d.framesDecoded, Detail: "part2_3_length exceeds available main data"}
	}
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
				return wrapFrameErr(err, d.framesDecoded)
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
			// mixed_block_flag 置位时最低子带仍用长窗,起始、短、结束块皆然
			nLongSub := 0
			if gi.windowSwitching && gi.mixedBlock {
				nLongSub = 2
				if fh.version == MPEG25 && fh.srIdx == 2 {
					nLongSub = 4 // MPEG-2.5 8 kHz 混合块;取值来自实现观察,未经标准文本核实
				}
			}
			d.chState[ch].imdctBlock(&d.spec[ch].xr, bt, nLongSub, bm[ch], &d.td)
			synthGranule(&d.synthSt[ch], &d.td, out[gr*576*nch+ch:], nch)
		}
	}
	d.rsv.advance(mainData)
	d.pcm = d.pcm[:len(d.pcm)+n] // 整帧解码成功才计入队列
	d.s.drop(fh.frameSize)
	d.framesDecoded++
	d.pcmTotal += int64(fh.samplesPerFrame)
	// gapless 头裁:标签帧之后的输出开头丢弃 enc_delay+decoderDelay 个采样帧
	if d.startSkip > 0 {
		skip := min(d.startSkip, (len(d.pcm)-d.pcmOff)/nch)
		d.pcmOff += skip * nch
		d.startSkip -= skip
	}
	return nil
}

// pcmReserve 确保 PCM 队列尾部有 n 个空位并返回该区间(尚未计入队列)。
// 已交付样本只在空间不足时整体搬走,未交付样本至多为 gapless 尾部缓冲加
// 一帧,队列容量因此有界。
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

// trailingBytesErr 归类流末尾不足一个帧头(1–3 字节)的数据:以帧同步位
// 开头视为被截断的帧头,否则为未知尾部数据。
func (d *Decoder) trailingBytesErr(b []byte) error {
	if b[0] == 0xFF && (len(b) < 2 || b[1]&0xE0 == 0xE0) {
		return &TruncatedError{Offset: d.s.tell(), What: "frame header"}
	}
	return &CorruptError{Frame: d.framesDecoded, Detail: "unknown trailing data after frame"}
}

const id3v1Size = 128

func isID3v1Start(b []byte) bool {
	return len(b) >= 3 && string(b[:3]) == "TAG"
}

// skipID3v1 跳过流末尾的 ID3v1 标签:标签须恰好 128 字节且其后即流结束;
// 不完整为截断,其后仍有数据为格式错误。
func (d *Decoder) skipID3v1(ctx context.Context) error {
	if err := d.s.fill(ctx, id3v1Size+1); err != nil {
		return err
	}
	switch {
	case len(d.s.buf) < id3v1Size:
		return &TruncatedError{Offset: d.s.tell(), What: "ID3v1 tag"}
	case len(d.s.buf) > id3v1Size:
		return &CorruptError{Frame: d.framesDecoded, Detail: "unknown data after ID3v1 tag"}
	}
	d.s.drop(id3v1Size)
	return d.finish()
}

// finish 在流结束时做尾部校验并应用 gapless 尾裁。
func (d *Decoder) finish() error {
	if d.xing != nil && d.xing.frames != nil && d.framesDecoded != int64(*d.xing.frames) {
		return &TruncatedError{Offset: d.s.tell(), What: "audio frame count vs Xing declaration"}
	}
	if d.vbri != nil && d.framesDecoded != int64(d.vbri.frames) {
		return &TruncatedError{Offset: d.s.tell(), What: "audio frame count vs VBRI declaration"}
	}
	if d.trimming && d.endTrim > 0 {
		nch := d.Channels()
		trim := d.endTrim * nch
		if trim > len(d.pcm)-d.pcmOff {
			return &CorruptError{Frame: -1, Detail: "gapless padding exceeds decoded length"}
		}
		d.pcm = d.pcm[:len(d.pcm)-trim]
	}
	d.finished = true
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
		// 流未正常结束时扣留 gapless 尾裁余量,待 finish 裁决
		hold := 0
		if !d.finished {
			hold = d.holdback
		}
		avail := (len(d.pcm)-d.pcmOff)/nch - hold
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
		// 解码出错:已缓冲样本照常交付,错误排空后返回;尾裁只适用于正常结束的流
		if err := d.decodeFrame(ctx); err != nil {
			d.termErr = err
			d.finished = true
			d.holdback = 0
		}
	}
}

// DecodedFrames 返回原始解码的 PCM 采样帧数(不含 gapless 裁剪)。
func (d *Decoder) DecodedFrames() int64 { return d.pcmTotal }

// Frames 返回实际交付的 PCM 采样帧数(gapless 裁剪后);io.EOF 后为最终值。
func (d *Decoder) Frames() int64 { return d.delivered }

// Trimmed 报告是否应用了 gapless 裁剪;为 false 时 Frames 代表解码输出
// 长度,不代表编码前音频长度。
func (d *Decoder) Trimmed() bool { return d.trimming }

// wrapFrameErr 给解码深处产生的 CorruptError 补上帧序号。
func wrapFrameErr(err error, frame int64) error {
	var c *CorruptError
	if errors.As(err, &c) {
		c.Frame = frame
	}
	return err
}
