package gl3d

import (
	"bytes"
	"context"
	"errors"
	"io"
)

const (
	tagSkipBudget    = 16 << 20
	syncSearchBudget = 64 << 10
)

// framer 把字节流切分为音频帧:跳过开头的 ID3v2 标签、搜索并确认首帧、解析
// Xing/Info/VBRI 标签帧,之后逐帧确认帧边界,并识别帧之间与音频之后的标签。
// 解码核心只处理它交出的帧。
type framer struct {
	s           *stream
	strict      bool
	rep         Report      // 报告:分帧层与解码器记下的异常、标签与计数。帧数在 Decoder.Report 中补齐
	header      frameHeader // 首帧确定格式,后续帧必须一致
	frames      int64       // 已交出的音频帧数,不含标签帧
	pending     int         // 上一次交出的帧长。这一帧到下一次调用 next 时才丢弃,所以交出的帧在解码期间有效
	tagBytes    int64       // 已跳过的标签字节数。严格模式下受 tagSkipBudget 约束,宽松模式超出只记入报告
	gap         bool        // 跳过了标签帧、垃圾或丢弃了帧,其后的帧尚未交出
	unconfirmed bool        // 首帧在源出错时未经确认就被接受,尚未交出
}

// audioFrame 是分帧层交出的一个音频帧。
type audioFrame struct {
	data        []byte // 整帧,引用内部缓冲,在下一次调用 next 前有效
	header      frameHeader
	gap         bool // 与上一帧之间跳过了标签帧、垃圾或丢弃了帧,所以此前的 main data 不能回指
	unconfirmed bool // 源出错时未经确认交出的首帧。这一帧解码失败时报告源错误
}

// errEndOfAudio 表示音频正常结束:流在帧边界结束,或其后只有可识别的尾部标签。
var errEndOfAudio = errors.New("gl3d: end of audio")

// streamInfo 是打开流时由首帧与标签帧确定的信息。
type streamInfo struct {
	header frameHeader
	xing   *xingInfo
	vbri   *vbriInfo
	lame   *lameTag
}

func newFramer(r io.Reader, strict bool) *framer { return &framer{s: newStream(r), strict: strict} }

// open 跳过开头的标签,定位首个音频帧并确定格式。首帧是标签帧时解析后整帧跳过。
func (f *framer) open(ctx context.Context) (streamInfo, error) {
	if err := f.skipTags(ctx); err != nil {
		return streamInfo{}, err
	}
	if err := f.syncSearch(ctx); err != nil {
		return streamInfo{}, err
	}
	// syncSearch 返回时 buf 起始是通过帧头校验的候选
	h, err := parseFrameHeader([4]byte(f.s.buf[:4]))
	if err != nil {
		return streamInfo{}, err
	}
	f.header = h
	f.rep.Emphasis = h.emphasis
	if h.emphasis != 0 {
		if f.unconfirmed {
			// 未经确认的候选再带 emphasis,多半是伪同步,所以与解码失败同理,不交出它,
			// 报告源错误
			return streamInfo{}, f.s.err
		}
		if err := f.emphasis(f.s.tell()); err != nil {
			return streamInfo{}, err
		}
	}
	info := streamInfo{header: h}
	if err := f.readMetaFrame(ctx, &info); err != nil {
		return streamInfo{}, err
	}
	return info, nil
}

// skipTags 跳过流开头连续的 ID3v2 标签(2.2/2.3/2.4)。标签声明的长度不可信,所以分块跳过,
// 不按声明长度分配内存,严格模式下累计跳过量以 tagSkipBudget 为限。头无效的 "ID3"
// 在严格模式报错。宽松模式不信这个标识,交给 syncSearch 当垃圾往后找帧。数据在标签
// 中间结束时宽松模式报无音频,因为其后没有东西可解码。
func (f *framer) skipTags(ctx context.Context) error {
	for {
		if err := f.s.fill(ctx, 10); err != nil {
			return err
		}
		if len(f.s.buf) < 3 || !bytes.Equal(f.s.buf[:3], []byte("ID3")) {
			return nil
		}
		if len(f.s.buf) < 10 {
			return f.cutShort(&TruncatedError{Offset: f.s.tell(), What: "ID3v2 header"})
		}
		size, footer, err := parseID3v2Header([10]byte(f.s.buf[:10]))
		if err != nil {
			// 宽松模式不信这个标识,所以这些字节按垃圾处理,由 syncSearch 往后找帧
			cerr := &CorruptError{Frame: -1, Detail: "invalid ID3v2 header: " + err.Error()}
			return f.tolerate(cerr, f.s.tell())
		}
		total := size
		if footer {
			total += 10
		}
		if err := f.chargeTag(int64(total), f.s.tell()); err != nil {
			return err
		}
		ok, err := f.s.skip(ctx, int64(total))
		if err != nil {
			return err
		}
		if !ok {
			return f.cutShort(&TruncatedError{Offset: f.s.tell(), What: "ID3v2 tag"})
		}
	}
}

// cutShort 处理打开流时数据在帧之前就结束的情形。严格模式报告 err。宽松模式下其后
// 没有可交出的数据,与没有任何帧的流相同,所以报无音频。
func (f *framer) cutShort(err error) error {
	if f.strict {
		return err
	}
	return ErrNoAudio
}

// chargeTag 把即将跳过的、属于 at 处标签的 n 字节计入预算。宽松模式不设累计上限,
// 超出只记入报告。
func (f *framer) chargeTag(n, at int64) error {
	if f.tagBytes+n > tagSkipBudget {
		if err := f.tolerate(&LimitError{Budget: "tag skip 16 MiB"}, at); err != nil {
			return err
		}
	}
	f.tagBytes += n
	return nil
}

// syncSearch 在 syncSearchBudget 内搜索首个有效音频帧,只读取确认所需的数据。同步位
// 只产生候选。候选还须通过帧头校验,并由帧长处的数据确认,否则伪同步会被误认为帧起点。
func (f *framer) syncSearch(ctx context.Context) error {
	searched := 0
	for {
		if err := f.s.fill(ctx, 2); err != nil {
			return err
		}
		w := f.s.buf
		if len(w) == 0 {
			return ErrNoAudio
		}
		i := -1
		for j := 0; j+1 < len(w); j++ {
			if w[j] == 0xFF && w[j+1]&0xE0 == 0xE0 {
				i = j
				break
			}
		}
		if i < 0 {
			// 丢弃已确认无同步的字节,保留可能是半截同步的末尾 0xFF。预算先于
			// EOF 判定,因为末段数据是否与 EOF 同一次到达取决于读取分块,不能影响结果
			drop := len(w)
			if w[len(w)-1] == 0xFF {
				drop--
			}
			if searched+drop > syncSearchBudget {
				return &LimitError{Budget: "sync search 64 KiB"}
			}
			if f.s.eof {
				if drop < len(w) {
					return f.cutShort(&TruncatedError{Offset: f.s.tell() + int64(drop), What: "frame header"})
				}
				return ErrNoAudio
			}
			searched += drop
			f.s.drop(drop)
			continue
		}
		if i > 0 {
			searched += i
			f.s.drop(i)
			if searched > syncSearchBudget {
				return &LimitError{Budget: "sync search 64 KiB"}
			}
		}
		if err := f.s.fill(ctx, 4); err != nil {
			return err
		}
		if len(f.s.buf) < 4 {
			return f.cutShort(&TruncatedError{Offset: f.s.tell(), What: "frame header"})
		}
		h, err := parseFrameHeader([4]byte(f.s.buf[:4]))
		switch u := err.(type) {
		case nil:
			ok, err := f.firstFrameConfirmed(ctx, h)
			if err != nil || ok {
				return err
			}
		case *UnsupportedError:
			// 范围外格式的同参数帧头在帧长处复现(free format 的帧长不定,在窗口内找
			// 复现),说明输入是该格式的流而非伪同步,报告不支持。否则按垃圾数据继续搜索
			ok, err := f.outOfRangeConfirmed(ctx)
			if err != nil {
				return err
			}
			if ok {
				return u
			}
		}
		searched++
		f.s.drop(1)
		if searched > syncSearchBudget {
			return &LimitError{Budget: "sync search 64 KiB"}
		}
	}
}

// firstFrameConfirmed 报告 buf 起点处的候选首帧是否确认。满足任一条件即算确认:帧长处
// 是有效帧头,流在帧和其后的 4 字节帧头到齐之前结束(帧体是否完整由 next 判定),或帧后
// 是可识别的标签。单独的 ID3v1 只有 3 字节标识,所以帧后是 ID3v1 时,还要求 ID3v1 之后
// 即流结束。
func (f *framer) firstFrameConfirmed(ctx context.Context, h frameHeader) (bool, error) {
	need := h.frameSize + 4
	if err := f.s.fill(ctx, need); err != nil {
		return f.unconfirmedFirst(err, h)
	}
	if len(f.s.buf) < need {
		return true, nil
	}
	if _, err := parseFrameHeader([4]byte(f.s.buf[h.frameSize:])); err == nil {
		return true, nil
	}
	// 帧后的 4 字节已经不是帧头,没有理由再接受未经确认的候选。流结束时帧后不是可识别的
	// 标签,候选就不成立,搜索继续。源出错则中止搜索,报告源错误
	if err := f.s.fill(ctx, h.frameSize+tagProbeSize); err != nil {
		return false, err
	}
	next := f.s.buf[h.frameSize:]
	switch kind := tagAt(next); kind {
	case 0:
		return false, nil
	case TagID3v1:
		return len(next) == id3v1Size && f.s.eof, nil
	}
	return true, nil
}

// unconfirmedFirst 处理读取候选之后的帧头时的错误。源出错而候选已完整到达时按未确认
// 接受,先交出,错误留到下一次读取。取消与其余情形原样返回。fill 在 s.err 已设置时
// 一律返回 s.err,取消只在 s.err 为空时才返回,所以 s.err 非空就说明 err 是源错误。
// 不拿 err 与 s.err 比较,因为源错误的动态类型可能不可比较。
func (f *framer) unconfirmedFirst(err error, h frameHeader) (bool, error) {
	if f.s.err != nil && len(f.s.buf) >= h.frameSize {
		f.unconfirmed = true
		return true, nil
	}
	return false, err
}

// outOfRangeConfirmed 报告 buf 起点处范围外格式的帧头是否在帧长处以相同参数复现,
// 或流恰在帧后结束。free format 的帧长不定,所以在 unsupportedConfirmWindow 内找复现。
func (f *framer) outOfRangeConfirmed(ctx context.Context) (bool, error) {
	size := layerFrameSize([4]byte(f.s.buf[:4]))
	if size == 0 {
		if err := f.s.fill(ctx, unsupportedConfirmWindow); err != nil {
			return false, err
		}
		return headerRecurs(f.s.buf, unsupportedConfirmWindow), nil
	}
	if err := f.s.fill(ctx, size+4); err != nil {
		return false, err
	}
	if b := f.s.buf; len(b) >= size+4 {
		return sameParams(b, b[size:]), nil
	}
	return len(f.s.buf) == size, nil
}

// unsupportedConfirmWindow 是确认 free format 帧头复现的窗口。这类帧长不由帧头给出,
// 实用中不超过约 2.9 KB。
const unsupportedConfirmWindow = 4096

// headerRecurs 报告 b[:4] 的帧头是否在 window 字节内以相同参数再次出现。
func headerRecurs(b []byte, window int) bool {
	if len(b) > window {
		b = b[:window]
	}
	for i := 4; i+4 <= len(b); i++ {
		if sameParams(b, b[i:]) {
			return true
		}
	}
	return false
}

// sameParams 报告 b 处的帧头是否与 a 处的帧头有相同的版本、层、保护位、码率、采样率、
// 声道模式、版权/原创位与 emphasis。逐帧可变的 padding、private 与 mode extension
// 位不比较。
func sameParams(a, b []byte) bool {
	return b[0] == 0xFF && b[1] == a[1] && b[2]&0xFC == a[2]&0xFC && b[3]&0xCF == a[3]&0xCF
}

// metaMagic 返回 frame 中 Xing/Info 或 VBRI 标签的标识及其偏移,没有时返回空串。Xing/Info
// 在帧头后 side information 长度处,帧头带 CRC 时也不加 CRC 的 2 字节。VBRI 固定在帧头后
// 32 字节。帧须容得下标签的定长部分。
func metaMagic(frame []byte, h frameHeader) (string, int) {
	if xo := 4 + h.sideInfoSize; len(frame) >= xo+8 {
		switch string(frame[xo : xo+4]) {
		case "Xing":
			return "Xing", xo
		case "Info":
			return "Info", xo
		}
	}
	if vo := 4 + 32; len(frame) >= vo+26 && string(frame[vo:vo+4]) == "VBRI" {
		return "VBRI", vo
	}
	return "", 0
}

// readMetaFrame 检查首帧是否为 Xing/Info 或 VBRI 标签帧。是标签帧就把解析结果写入
// info 并整帧跳过,因为标签帧不作为音频解码,也不计入解码帧数。
func (f *framer) readMetaFrame(ctx context.Context, info *streamInfo) error {
	h := f.header
	if err := f.s.fill(ctx, h.frameSize); err != nil {
		return err
	}
	if len(f.s.buf) < h.frameSize {
		return nil // EOF 短流,由 next 报告截断
	}
	frame := f.s.buf[:h.frameSize]
	switch magic, off := metaMagic(frame, h); magic {
	case "Xing", "Info":
		x, lameOff, err := parseXing(frame[off:])
		if err != nil {
			cerr := &CorruptError{Frame: 0, Detail: "malformed Xing/Info header: " + err.Error()}
			return f.metaMalformed(cerr, h.frameSize)
		}
		// 带 LAME 扩展时标签 CRC 覆盖 Info 帧中 CRC 字段之前的全部字节。CRC 不符则
		// 帧数与 gapless 字段都不可信,所以整帧仍按标签帧跳过,但不采用其字段
		l, hasLAME := parseLAMETag(frame[off+lameOff:])
		if !hasLAME || infoTagCRCValid(frame, off+lameOff+34) {
			info.xing = x
			if hasLAME {
				info.lame = l
			}
		}
		f.s.drop(h.frameSize)
		f.unconfirmed = false // 标签帧不会成为音频,未确认标记随它一起丢弃
	case "VBRI":
		v, err := parseVBRI(frame[off:])
		if err != nil {
			cerr := &CorruptError{Frame: 0, Detail: "malformed VBRI header: " + err.Error()}
			return f.metaMalformed(cerr, h.frameSize)
		}
		info.vbri = v
		f.s.drop(h.frameSize)
		f.unconfirmed = false
	}
	return nil
}

// metaMalformed 处理首帧里解析不了的 Xing/Info/VBRI 标签。未经确认的候选带着这样的
// 异常多半是伪同步,所以不交出它,两种模式都报告源错误。其余情形严格模式返回 err。宽松
// 模式记入报告,整帧按标签帧丢弃,不采用其中任何字段,其后的数据从下一帧起照常处理。
func (f *framer) metaMalformed(err error, size int) error {
	if f.unconfirmed {
		return f.s.err
	}
	if err := f.tolerate(err, f.s.tell()); err != nil {
		return err
	}
	f.s.drop(size)
	return nil
}

// next 交出下一个完整音频帧。帧后只能是流结束、下一帧帧头或可识别的标签。标签在
// 下一次调用时逐个跳过,其后是流结束即为尾部标签。否则这串标签不在流尾(见 closeRun)。
// 这时如果其后是可以确认的帧,异常是流中间的标签,否则异常是标签之后的未知数据。这两种
// 异常、其余情形与 Xing/Info/VBRI 标签帧,严格模式都报错。宽松模式记入报告后跳过,这串
// 标签一律计为流中间的标签,损坏处从后面的数据重新同步,跳过标签帧或损坏之后交出的帧
// 带断档标记。流正常结束时返回 errEndOfAudio。
func (f *framer) next(ctx context.Context) (audioFrame, error) {
	f.s.drop(f.pending)
	f.pending = 0
	var run tagRun
	for {
		if err := ctx.Err(); err != nil {
			return audioFrame{}, err
		}
		// 帧长以本帧帧头为准(VBR 逐帧可变),先读 4 字节帧头
		if err := f.s.fill(ctx, 4); err != nil {
			return audioFrame{}, err
		}
		if len(f.s.buf) == 0 {
			f.rep.TrailingTags |= run.kinds
			return audioFrame{}, errEndOfAudio
		}
		if !frameAt(f.s.buf) {
			if err := f.s.fill(ctx, tagProbeSize); err != nil {
				return audioFrame{}, err
			}
			if kind := tagAt(f.s.buf); kind != 0 {
				if err := f.skipOneTag(ctx, kind, &run); err != nil {
					return audioFrame{}, err
				}
				continue
			}
		}
		if run.n > 0 {
			if err := f.closeRun(ctx, &run); err != nil {
				return audioFrame{}, err
			}
		}
		af, ok, err := f.frame(ctx)
		if err != nil {
			return audioFrame{}, err
		}
		if ok {
			return af, nil
		}
	}
}

// skipOneTag 跳过 buf 起点处 kind 类的标签并记入 run。标签损坏时严格模式返回错误。
// 宽松模式把已经读过的部分算作跳过的数据,从这里重新同步,这串标签中此前读完的计为流中间
// 的标签。
func (f *framer) skipOneTag(ctx context.Context, kind TagSet, run *tagRun) error {
	at := f.s.tell()
	if err := f.skipTag(ctx, kind); err != nil {
		earlier := run.n
		*run = tagRun{}
		if err := f.skipBroken(ctx, err, at, f.s.tell()-at); err != nil {
			return err
		}
		// 没有止于流尾的一串标签是流中间的标签,与 closeRun 相同。严格模式在上面已经返回
		f.rep.MidstreamTags += earlier
		return nil
	}
	run.add(kind, at)
	return nil
}

// closeRun 结束 buf 起点之前的一串标签。其后还有数据,所以这些标签不在流尾。其后是帧尾
// 能确认的帧(不放宽间隙)时,异常是流中间的标签,指向第一个标签。只凭一个帧头不算,因为
// 这个帧头也可能是垃圾的开头。不能确认时,异常是标签之后的未知数据,指向最后一个标签。
// 严格模式返回这个异常。宽松模式记入报告,把这串标签计为流中间的标签,按长度跳过,
// bit reservoir 保留,其后的数据由 frame 处理:完好的帧照常交出,垃圾从后面重新同步。
func (f *framer) closeRun(ctx context.Context, run *tagRun) error {
	// 源出错或取消时按已经到达的数据判断。错误留给 frame,frame 先交出完整的帧,与没有
	// 标签时相同
	if err := f.s.fill(ctx, resyncWindow); err != nil && len(f.s.buf) < 4 {
		return err
	}
	var err error
	at := run.at
	if f.candidateAt(f.s.buf, 0) {
		err = &CorruptError{Frame: f.frames, Detail: run.first.String() + " tag in mid-stream"}
	} else {
		err = &CorruptError{Frame: f.frames, Detail: "unknown data after " + run.last.String() + " tag"}
		at = run.lastAt
	}
	if err := f.tolerate(err, at); err != nil {
		return err
	}
	f.rep.MidstreamTags += run.n
	*run = tagRun{}
	return nil
}

// frame 处理 buf 起点的帧。这一帧完整、格式与流相同、帧后得到确认时交出它(ok 为真)。
// 帧后不能确认时,宽松模式在帧内找到重新同步的候选才丢弃这一帧,找不到就照常交出。源出错
// 或取消而无从查看帧后时,两种模式都先交出,错误留到下一轮。其余情形严格模式报错。
// 宽松模式记入报告,跳过标签帧,或丢弃损坏的部分从后面重新同步,返回 ok 为假,由 next
// 继续。
func (f *framer) frame(ctx context.Context) (af audioFrame, ok bool, err error) {
	if len(f.s.buf) < 4 {
		return audioFrame{}, false, f.skipBroken(ctx, f.trailingBytesErr(f.s.buf, f.s.tell()), f.s.tell(), 0)
	}
	fh, err := parseFrameHeader([4]byte(f.s.buf[:4]))
	if err != nil {
		return audioFrame{}, false, f.skipBroken(ctx, wrapFrameErr(err, f.frames), f.s.tell(), 0)
	}
	if fh.emphasis != 0 {
		if err := f.emphasis(f.s.tell()); err != nil {
			return audioFrame{}, false, err
		}
	}
	// 版本、采样率、声道数中途改变属范围外格式。码率、padding 与 emphasis 可逐帧变化
	if !f.sameFormat(fh) {
		return audioFrame{}, false, f.skipBroken(ctx, &UnsupportedError{Feature: "mid-stream format change"}, f.s.tell(), 0)
	}
	// 预取下一帧帧头用于同步确认。源出错或取消时若本帧已完整,先交出本帧,
	// 错误留到下一轮报告,不吞掉最后一批样本
	lookErr := f.s.fill(ctx, fh.frameSize+4)
	if len(f.s.buf) < fh.frameSize {
		if lookErr != nil {
			return audioFrame{}, false, lookErr
		}
		return audioFrame{}, false, f.skipBroken(ctx, &TruncatedError{Offset: f.s.tell(), What: "frame body"}, f.s.tell(), 0)
	}
	if magic := tagFrameMagic(f.s.buf[:fh.frameSize], fh); magic != "" {
		if err := f.tolerate(&CorruptError{Frame: f.frames, Detail: magic + " tag frame in mid-stream"}, f.s.tell()); err != nil {
			return audioFrame{}, false, err
		}
		f.rep.MidstreamTags++
		f.s.drop(fh.frameSize)
		f.gap = true
		return audioFrame{}, false, nil
	}
	if lookErr == nil {
		dropped, err := f.confirmEnd(ctx, fh)
		if err != nil || dropped {
			return audioFrame{}, false, err
		}
	}
	af = audioFrame{data: f.s.buf[:fh.frameSize], header: fh, gap: f.gap, unconfirmed: f.unconfirmed}
	f.gap, f.unconfirmed = false, false
	f.pending = fh.frameSize
	f.frames++
	return af, true, nil
}

// confirmEnd 确认 buf 起点长 fh.frameSize 的帧之后是流结束、下一帧帧头(含已识别的
// 范围外格式,下一轮再处理)或可识别的标签(下一轮解析)。不能确认时严格模式报错。
// 宽松模式在帧内找重新同步的候选。找到候选说明这一帧丢了字节、帧长指向了错误的位置,
// 这时丢弃这一帧,返回 true。找不到则这一帧完好,照常交出,帧后的数据在下一轮跳过。
// 源出错或取消时照常交出,错误留到下一轮。
func (f *framer) confirmEnd(ctx context.Context, fh frameHeader) (bool, error) {
	next := f.s.buf[fh.frameSize:]
	if len(next) == 0 || frameAt(next) {
		return false, nil
	}
	if err := f.s.fill(ctx, fh.frameSize+tagProbeSize); err != nil {
		return false, nil
	}
	next = f.s.buf[fh.frameSize:]
	if tagAt(next) != 0 {
		return false, nil
	}
	var err error = &CorruptError{Frame: f.frames, Detail: "lost sync after frame"}
	if len(next) < 4 {
		err = f.trailingBytesErr(next, f.s.tell()+int64(fh.frameSize))
	}
	if err := f.tolerate(err, f.s.tell()); err != nil {
		return false, err
	}
	p := f.candidateWithin(ctx, fh.frameSize)
	if p == 0 {
		return false, nil
	}
	f.s.drop(p)
	f.rep.SkippedBytes += int64(p)
	f.rep.DroppedFrames++
	f.rep.Resyncs++
	f.gap = true
	return true, nil
}

// frameAt 报告 b 起点是否是帧头,包括已识别的范围外格式。
func frameAt(b []byte) bool {
	// 不带帧同步位的字节占多数,parseFrameHeader 对它们每次都构造一个错误
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return false
	}
	_, err := parseFrameHeader([4]byte(b))
	if err == nil {
		return true
	}
	_, ok := err.(*UnsupportedError)
	return ok
}

// tagRun 是帧之间连续的一串标签。
type tagRun struct {
	n           int64
	kinds       TagSet
	first, last TagSet
	at, lastAt  int64 // 第一个与最后一个标签的起点
}

func (r *tagRun) add(kind TagSet, at int64) {
	if r.n == 0 {
		r.first, r.at = kind, at
	}
	r.n++
	r.kinds |= kind
	r.last, r.lastAt = kind, at
}

// tolerate 把一处宽松模式可以跳过的异常记入报告。严格模式下返回 err 终止解码,
// 宽松模式下返回 nil,由调用方跳过该结构继续。
func (f *framer) tolerate(err error, at int64) error {
	f.rep.note(err, f.frames, at)
	if f.strict {
		return err
	}
	return nil
}

// skipBroken 处理一处宽松模式跳过的损坏,先把它记入报告。严格模式返回 err。宽松模式
// 把损坏处已经读过的 consumed 字节算作跳过的数据,从 buf 起点重新同步。读取错误与
// 取消不是流的结构,两种模式都原样返回。
func (f *framer) skipBroken(ctx context.Context, err error, at, consumed int64) error {
	kind := issueKind(err)
	if kind == 0 {
		return err
	}
	if err := f.tolerate(err, at); err != nil {
		return err
	}
	f.rep.SkippedBytes += consumed
	if consumed == 0 {
		// 损坏处一个字节也没有读过,buf 起点仍是它的开头(如读不下去的标签)。所以先
		// 跳过起点的一个字节,否则重新同步会在原处再次认出它,无法前进。起点是
		// APETAGEX 时跳过整个标识,因为只跳一个字节的话,APETAGEX 里的 "TAG" 会被当作
		// ID3v1,吃掉其后的 128 字节
		n := 1
		if bytes.HasPrefix(f.s.buf, apeMagic) {
			n = len(apeMagic)
		}
		f.s.drop(n)
		f.rep.SkippedBytes += int64(n)
	}
	if kind == IssueTruncated {
		f.rep.Truncated = true
	}
	return f.resync(ctx)
}

// maxFrameSize 是 Layer III 的最大帧长:MPEG-1 320 kbps @ 32 kHz 或 MPEG-2.5
// 160 kbps @ 8 kHz,加 padding。
const maxFrameSize = 1441

// resyncWindow 是检查一个重新同步候选要看的字节数:最大帧长,加上确认帧后结构所需的
// tagProbeSize。
const resyncWindow = maxFrameSize + tagProbeSize

// resyncSlack 是重新同步确认候选时,帧与下一帧的帧头之间允许夹着的垃圾字节数。帧之间
// 被插入几个字节时,每一帧的帧长处都落在垃圾上,按帧长精确确认会把这一串完好的帧全部
// 丢掉。FFmpeg 与 mpg123 也在下一个帧头处恢复。垃圾里要同时有两个靠得这么近、格式相同
// 的帧头才会误认。松弛只用于重新同步,其余检查仍按精确的帧长。candidateWithin 在帧内找
// 候选时也不用松弛,因为丢了字节的帧恰好结束在下一帧的起点,帧之间没有夹着垃圾。
const resyncSlack = 32

// 候选加松弛字节再加下一个帧头须落在 resyncWindow 之内
const _ = uint(resyncWindow - maxFrameSize - resyncSlack - 4)

// mayStart 标出可能是帧或带标识的标签起点的字节:帧同步与 TAG、ID3、APETAGEX、
// LYRICSBEGIN 的首字母。
var mayStart = [256]bool{0xFF: true, 'T': true, 'I': true, 'A': true, 'L': true}

// resync 在宽松模式下跳过 buf 起点处不能解码的数据,停在候选帧、带标识的标签或流
// 结束处。跳过的字节记入报告。停下之后交出的第一帧带断档标记。扫描不限距离,每一
// 步只看有界的缓冲,可以取消。
func (f *framer) resync(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.s.fill(ctx, resyncWindow); err != nil {
			return err
		}
		b := f.s.buf
		if len(b) == 0 {
			return nil
		}
		if b[0] == 0xFF && f.candidateAt(b, resyncSlack) || magicTagAt(b) != 0 {
			f.rep.Resyncs++
			f.gap = true
			return nil
		}
		n := 1
		if bytes.HasPrefix(b, apeMagic) {
			n = len(apeMagic) // 头无效的 APETAGEX:其中的 "TAG" 不是 ID3v1
		}
		for n < len(b) && !mayStart[b[n]] {
			n++
		}
		f.s.drop(n)
		f.rep.SkippedBytes += int64(n)
	}
}

// candidateAt 报告 b 起点是否是重新同步可以接受的帧:格式与流相同的帧头,且帧长处
// 是同格式的帧头、可识别的标签或流结束。slack 大于零时,帧后至多 slack 字节内才出现
// 同格式帧头也算。b 应含 resyncWindow 字节,流尾不足时为剩余的全部。
func (f *framer) candidateAt(b []byte, slack int) bool {
	if !plausibleLayer3(b) {
		return false
	}
	h, err := parseFrameHeader([4]byte(b))
	if err != nil || !f.sameFormat(h) || len(b) < h.frameSize {
		return false
	}
	next := b[h.frameSize:]
	if len(next) == 0 {
		return f.s.eof
	}
	if f.headerAt(next) || tagAt(next) != 0 {
		return true
	}
	for i := 1; i <= slack && i < len(next); i++ {
		if next[i] == 0xFF && f.headerAt(next[i:]) {
			return true
		}
	}
	return false
}

// headerAt 报告 b 起点是否是与流同格式的 Layer III 帧头。
func (f *framer) headerAt(b []byte) bool {
	if !plausibleLayer3(b) {
		return false
	}
	h, err := parseFrameHeader([4]byte(b))
	return err == nil && f.sameFormat(h)
}

// candidateWithin 在 buf 起点处长 size 字节的帧内部寻找重新同步的候选,返回其偏移。
// 找不到,或源出错、取消而无从查看时返回 0。
func (f *framer) candidateWithin(ctx context.Context, size int) int {
	if err := f.s.fill(ctx, size+resyncWindow); err != nil {
		return 0
	}
	b := f.s.buf
	for p := 1; p < size && p < len(b); p++ {
		if b[p] == 0xFF && f.candidateAt(b[p:], 0) {
			return p
		}
	}
	return 0
}

// sameFormat 报告 h 是否与首帧的版本、采样率、声道数相同。
func (f *framer) sameFormat(h frameHeader) bool {
	return h.version == f.header.version && h.sampleRate == f.header.sampleRate && h.channels() == f.header.channels()
}

// errEmphasis 是 emphasis 非零的帧记入报告时用的错误。解码器不做去加重,所以这样的帧
// 输出与编码时的意图不同。宽松模式照常解码,与 FFmpeg 相同。每个这样的帧都要记一次,
// 共用一个值才不会逐帧分配。
var errEmphasis = &UnsupportedError{Feature: "non-zero emphasis"}

// emphasis 记下 at 处 emphasis 非零的帧。严格模式返回错误,宽松模式返回 nil 照常解码。
// 交给调用方的是新值,调用方改动它不影响别的解码器。
func (f *framer) emphasis(at int64) error {
	if err := f.tolerate(errEmphasis, at); err != nil {
		return &UnsupportedError{Feature: errEmphasis.Feature}
	}
	return nil
}

// tagFrameMagic 返回 frame 作为 Xing/Info 或 VBRI 标签帧的标识,不是标签帧时返回
// 空串。标识的位置与长度要求同首帧的识别(metaMagic)。此外帧头(及 CRC)与标识之间
// 须全为零,因为标签帧在这里不放 side information,音频帧放。
func tagFrameMagic(frame []byte, h frameHeader) string {
	magic, off := metaMagic(frame, h)
	if magic == "" {
		return ""
	}
	from := 4
	if h.hasCRC {
		from = 6
	}
	for _, c := range frame[from:off] {
		if c != 0 {
			return ""
		}
	}
	return magic
}

// offset 返回流中当前位置的字节偏移。解码某帧期间,这个位置就是该帧的起点。
func (f *framer) offset() int64 { return f.s.tell() }

// trailingBytesErr 归类流末尾 at 处不足一个帧头(1–3 字节)的数据:以帧同步位
// 开头视为被截断的帧头,否则为未知尾部数据。
func (f *framer) trailingBytesErr(b []byte, at int64) error {
	if b[0] == 0xFF && (len(b) < 2 || b[1]&0xE0 == 0xE0) {
		return &TruncatedError{Offset: at, What: "frame header"}
	}
	return &CorruptError{Frame: f.frames, Detail: "unknown trailing data after frame"}
}

const id3v1Size = 128

func isID3v1Start(b []byte) bool {
	return len(b) >= 3 && string(b[:3]) == "TAG"
}
