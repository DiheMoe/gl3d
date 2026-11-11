package gl3d

import (
	"context"
	"io"
)

// Decoder 从顺序字节流解码 MPEG Layer III 音频。同一实例只能由单个调用链
// 顺序使用,不是并发安全的。多个实例之间不共享可变状态,可以并发解码。
type Decoder struct {
	fr     *framer
	header frameHeader // 首帧的格式

	xing *xingInfo
	vbri *vbriInfo
	lame *lameTag

	rsv     *reservoir
	chState [2]channelState
	synthSt [2]synthState
	td      [32][18]float64 // IMDCT 输出暂存,两声道复用
	si      sideInfo
	spec    [2]granuleSpectrum
	prevScf [2][40]int // MPEG-1 granule 0 的 scalefactor,供 scfsi 复用
	pcm     []float64  // 交错样本队列,pcm[pcmOff:] 为待交付部分
	pcmOff  int

	// gapless 裁剪丢弃两个采样帧区间。头裁 [0, headTrim) 按解码序号(从第一个音频帧
	// 起计)。尾裁 [tailFrom, tailTo) 按帧在流中的位置,没有尾裁时 tailFrom 与 tailTo
	// 相等。解码时丢弃的帧已经确认过帧尾,不产出 PCM 而仍占位置,与 FFmpeg 相同。
	// 分帧层丢弃的数据可能是垃圾里的伪帧,不计位置,与 mpg123 相同
	headTrim, tailFrom, tailTo int64
	badPadding                 bool // 填充超出头裁之后的长度,所以不做尾裁,流结束时报告
	trimming                   bool

	framesDecoded int64 // 已解码音频帧数,不含标签帧
	dropped       int64 // 解码时丢弃的帧:side info 非法,或所需位数超出 main data
	pcmTotal      int64 // 已产出 PCM 采样帧数,含被裁剪部分
	delivered     int64
	finished      bool
	termErr       error
}

// SampleRate 返回流的采样率(Hz),保持输入原值,不重新采样。
func (d *Decoder) SampleRate() int { return d.header.sampleRate }

// Channels 返回声道数(1 或 2),保持输入流的声道数,不复制不下混。
func (d *Decoder) Channels() int { return d.header.channels() }

// NewDecoder 读取定位首个有效音频帧及确认格式所需的数据。
// 返回时采样率与声道数已确定,预读的数据不丢失,继续参与正常解码。
// r 由调用方拥有,解码器不会关闭它,也不启动后台 goroutine,
// 所有读取都发生在调用过程中。选项按传入顺序生效。
//
// 默认是宽松模式。宽松模式下,可以容忍的异常结构跳过并记入 Report,
// 不终止解码。Strict 选择严格模式。
func NewDecoder(ctx context.Context, r io.Reader, opts ...Option) (*Decoder, error) {
	cfg := config{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	fr := newFramer(r, cfg.strict)
	info, err := fr.open(ctx)
	if err != nil {
		return nil, err
	}
	d := &Decoder{fr: fr, header: info.header, xing: info.xing, vbri: info.vbri, lame: info.lame}
	maxRsv := 511
	if d.header.version.lsf() {
		maxRsv = 255
	}
	d.rsv = newReservoir(maxRsv)
	if d.lame != nil {
		d.trimming = true
		d.headTrim = int64(d.lame.encDelay + decoderDelay)
		// 尾裁位置由声明的帧数决定,与实际解出多少帧无关。没有声明时不做尾裁
		if n := d.declaredFrames(); n >= 0 {
			end := n * int64(d.header.samplesPerFrame)
			pad := int64(max(d.lame.encPadding-decoderDelay, 0))
			if pad > 0 && end-pad < d.headTrim {
				d.badPadding = true
			} else {
				d.tailFrom, d.tailTo = end-pad, end
			}
		}
	}
	return d, nil
}

// declaredFrames 返回 Xing 或 VBRI 声明的音频帧数。没有声明或声明为 0(未知)时返回 -1。
func (d *Decoder) declaredFrames() int64 {
	switch {
	case d.xing != nil && d.xing.frames != nil && *d.xing.frames != 0:
		return int64(*d.xing.frames)
	case d.vbri != nil && d.vbri.frames != 0:
		return int64(d.vbri.frames)
	}
	return -1
}
