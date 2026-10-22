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

// Decoder 从顺序字节流解码 MPEG Layer III 音频。同一实例只能由单个调用链
// 顺序使用,不是并发安全的;多个实例之间不共享可变状态,可以并发解码。
type Decoder struct {
	s      *stream
	header frameHeader // 首帧确定格式,后续帧必须一致

	xing *xingInfo
	vbri *vbriInfo
	lame *lameTag

	rsv       *reservoir
	chState   [2]channelState
	synthSt   [2]synthState
	td        [32][18]float64 // IMDCT 输出暂存,两声道复用
	si        sideInfo
	spec      [2]granuleSpectrum
	prevScf   [2][40]int // MPEG-1 granule 0 的 scalefactor,供 scfsi 复用
	pcm       []float64  // 交错样本队列,pcm[pcmOff:] 为待交付部分
	pcmOff    int
	startSkip int // gapless 头裁:待丢弃的采样帧数
	holdback  int // 到达流末尾前在尾部保留的采样帧数,供 gapless 尾裁
	endTrim   int // EOF 时从尾部裁掉的采样帧数
	trimming  bool

	framesDecoded int64 // 已解码音频帧数,不含标签帧
	pcmTotal      int64 // 已产出 PCM 采样帧数,含被裁剪部分
	delivered     int64
	finished      bool
	termErr       error
}

// SampleRate 返回流的采样率(Hz),保持输入原值,不重新采样。
func (d *Decoder) SampleRate() int { return d.header.sampleRate }

// Channels 返回声道数(1 或 2),保持输入流的声道数,不复制不下混。
func (d *Decoder) Channels() int { return d.header.channels() }

// NewDecoder 读取定位首个有效音频帧及确认格式所需的数据;返回时采样率与
// 声道数已确定,预读的数据不丢失,继续参与正常解码。r 由调用方拥有,解码器
// 不会关闭它,也不启动后台 goroutine,所有读取都发生在调用过程中。
func NewDecoder(ctx context.Context, r io.Reader) (*Decoder, error) {
	d := &Decoder{s: newStream(r)}
	if err := d.skipTags(ctx); err != nil {
		return nil, err
	}
	if err := d.syncSearch(ctx); err != nil {
		return nil, err
	}
	// syncSearch 已保证 buf 起始是确认过的帧头
	h, err := parseFrameHeader([4]byte(d.s.buf[:4]))
	if err != nil {
		return nil, err
	}
	d.header = h
	if err := d.readMetaFrame(ctx); err != nil {
		return nil, err
	}
	maxRsv := 511
	if d.header.version.lsf() {
		maxRsv = 255
	}
	d.rsv = newReservoir(maxRsv)
	if d.lame != nil {
		d.trimming = true
		d.startSkip = d.lame.encDelay + decoderDelay
		d.endTrim = d.lame.encPadding - decoderDelay
		if d.endTrim < 0 {
			d.endTrim = 0
		}
		d.holdback = d.endTrim
	}
	return d, nil
}

// skipTags 跳过流开头连续的 ID3v2 标签(2.2/2.3/2.4),累计跳过量以
// tagSkipBudget 为限。标签声明的长度不可信:分块跳过,不按声明长度分配内存。
func (d *Decoder) skipTags(ctx context.Context) error {
	var skipped int64
	for {
		if err := d.s.fill(ctx, 10); err != nil {
			return err
		}
		if len(d.s.buf) < 3 || !bytes.Equal(d.s.buf[:3], []byte("ID3")) {
			return nil
		}
		if len(d.s.buf) < 10 {
			return &TruncatedError{Offset: d.s.tell(), What: "ID3v2 header"}
		}
		size, footer, err := parseID3v2Header([10]byte(d.s.buf[:10]))
		if err != nil {
			return &CorruptError{Frame: -1, Detail: "invalid ID3v2 header: " + err.Error()}
		}
		total := size
		if footer {
			total += 10
		}
		if skipped+int64(total) > tagSkipBudget {
			return &LimitError{Budget: "tag skip 16 MiB"}
		}
		ok, err := d.s.skip(ctx, int64(total))
		if err != nil {
			return err
		}
		if !ok {
			return &TruncatedError{Offset: d.s.tell(), What: "ID3v2 tag"}
		}
		skipped += int64(total)
	}
}

// syncSearch 在 syncSearchBudget 内搜索首个有效音频帧。同步位只产生候选:
// 候选还须通过帧头校验,并在数据充足时由其 frameSize 处的下一帧帧头确认,
// 否则伪同步会被误认为帧起点。
func (d *Decoder) syncSearch(ctx context.Context) error {
	searched := 0
	for {
		if err := d.s.fill(ctx, 4096); err != nil {
			return err
		}
		w := d.s.buf
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
			if d.s.eof {
				if w[len(w)-1] == 0xFF {
					return &TruncatedError{Offset: d.s.tell() + int64(len(w)) - 1, What: "frame header"}
				}
				return ErrNoAudio
			}
			// 丢弃已确认无同步的字节,保留可能是半截同步的末尾 0xFF
			drop := len(w)
			if w[len(w)-1] == 0xFF {
				drop--
			}
			searched += drop
			d.s.drop(drop)
			if searched > syncSearchBudget {
				return &LimitError{Budget: "sync search 64 KiB"}
			}
			continue
		}
		if i > 0 {
			searched += i
			d.s.drop(i)
			if searched > syncSearchBudget {
				return &LimitError{Budget: "sync search 64 KiB"}
			}
		}
		if err := d.s.fill(ctx, 4); err != nil {
			return err
		}
		if len(d.s.buf) < 4 {
			return &TruncatedError{Offset: d.s.tell(), What: "frame header"}
		}
		h, err := parseFrameHeader([4]byte(d.s.buf[:4]))
		if err != nil {
			var u *UnsupportedError
			if errors.As(err, &u) {
				// 范围外格式:同参数帧头在窗口内复现,说明输入是该格式的流
				// 而非伪同步,报告不支持;否则按垃圾数据继续搜索
				if err := d.s.fill(ctx, unsupportedConfirmWindow); err != nil {
					return err
				}
				if headerRecurs(d.s.buf, unsupportedConfirmWindow) {
					return u
				}
			}
			searched++
			d.s.drop(1)
			if searched > syncSearchBudget {
				return &LimitError{Budget: "sync search 64 KiB"}
			}
			continue
		}
		if err := d.s.fill(ctx, h.frameSize+4); err != nil {
			return err
		}
		if len(d.s.buf) >= h.frameSize+4 {
			nx := d.s.buf[h.frameSize:]
			if _, err := parseFrameHeader([4]byte(nx[:4])); err == nil {
				return nil
			}
			searched++
			d.s.drop(1)
			if searched > syncSearchBudget {
				return &LimitError{Budget: "sync search 64 KiB"}
			}
			continue
		}
		// EOF 前数据不足以确认下一帧:按合法短流接受候选,帧体是否完整
		// 由帧循环判定
		return nil
	}
}

// unsupportedConfirmWindow 是确认范围外格式帧头复现的窗口,须覆盖这类格式
// 的最大帧长:Layer II 最高 384 kbps @ 32 kHz 为 1728 B,free format 实用
// 不超过约 2.9 KB。
const unsupportedConfirmWindow = 4096

// headerRecurs 报告 b[:4] 的帧头是否在 window 字节内以相同的版本、层、
// 保护位、码率、采样率、声道模式、版权/原创位与 emphasis 再次出现;逐帧
// 可变的 padding、private 与 mode extension 位不参与比较。
func headerRecurs(b []byte, window int) bool {
	if len(b) > window {
		b = b[:window]
	}
	for i := 4; i+4 <= len(b); i++ {
		if b[i] == 0xFF && b[i+1] == b[1] && b[i+2]&0xFC == b[2]&0xFC && b[i+3]&0xCF == b[3]&0xCF {
			return true
		}
	}
	return false
}

// readMetaFrame 检查首帧是否为 Xing/Info 或 VBRI 标签帧;是则解析其元信息
// 并整帧跳过:标签帧不作为音频解码,也不计入解码帧数。
func (d *Decoder) readMetaFrame(ctx context.Context) error {
	h := d.header
	if err := d.s.fill(ctx, h.frameSize); err != nil {
		return err
	}
	if len(d.s.buf) < h.frameSize {
		return nil // EOF 短流:帧循环将报告截断
	}
	frame := d.s.buf[:h.frameSize]
	if xo := 4 + h.sideInfoSize; len(frame) >= xo+8 {
		if magic := string(frame[xo : xo+4]); magic == "Xing" || magic == "Info" {
			x, off, err := parseXing(frame[xo:])
			if err != nil {
				return &CorruptError{Frame: 0, Detail: "malformed Xing/Info header: " + err.Error()}
			}
			// 带 LAME 扩展时标签 CRC 覆盖整个 Info 帧;不符则帧数与 gapless
			// 字段都不可信:整帧仍按标签帧跳过,但不采用其字段
			l, hasLAME := parseLAMETag(frame[xo+off:])
			if !hasLAME || infoTagCRCValid(frame, xo+off+34) {
				d.xing = x
				if hasLAME {
					d.lame = l
				}
			}
			d.s.drop(h.frameSize)
			return nil
		}
	}
	if vo := 4 + 32; len(frame) >= vo+26 && string(frame[vo:vo+4]) == "VBRI" {
		v, err := parseVBRI(frame[vo:])
		if err != nil {
			return &CorruptError{Frame: 0, Detail: "malformed VBRI header: " + err.Error()}
		}
		d.vbri = v
		d.s.drop(h.frameSize)
	}
	return nil
}
