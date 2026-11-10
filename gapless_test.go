package gl3d

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

// infoFrame 定位语料文件中的 Info/Xing 帧:返回帧起点、Xing 头与 LAME 扩展
// 在帧内的偏移;仅作为构造变异输入的测试夹具。
func infoFrame(t *testing.T, data []byte) (start, xo, lameOff int) {
	t.Helper()
	if string(data[:3]) == "ID3" {
		size, footer, err := parseID3v2Header([10]byte(data[:10]))
		if err != nil {
			t.Fatal(err)
		}
		start = size
		if footer {
			start += 10
		}
	}
	h, err := parseFrameHeader([4]byte(data[start : start+4]))
	if err != nil {
		t.Fatal(err)
	}
	xo = 4 + h.sideInfoSize
	_, off, err := parseXing(data[start+xo:])
	if err != nil {
		t.Fatal(err)
	}
	return start, xo, xo + off
}

// arcCRC 是 Info 标签使用的 CRC-16/ARC(反射多项式 0xA001、初值 0)。
func arcCRC(data []byte) uint16 {
	var c uint16
	for _, b := range data {
		c ^= uint16(b)
		for i := 0; i < 8; i++ {
			if c&1 != 0 {
				c = c>>1 ^ 0xA001
			} else {
				c >>= 1
			}
		}
	}
	return c
}

// setLAMETagCRC 按 LAME 的约定重算 Info 标签 CRC:覆盖 CRC 字段之前的全部字节。
func setLAMETagCRC(frame []byte, lameOff int) {
	pos := lameOff + 34
	binary.BigEndian.PutUint16(frame[pos:], arcCRC(frame[:pos]))
}

// setFFmpegTagCRC 按 FFmpeg 的约定重算 Info 标签 CRC:覆盖帧首 190 字节,CRC 字段
// 置 0,帧尾之后按 0 计。
func setFFmpegTagCRC(frame []byte, lameOff int) {
	pos := lameOff + 34
	var buf [190]byte
	copy(buf[:], frame)
	buf[pos], buf[pos+1] = 0, 0
	binary.BigEndian.PutUint16(frame[pos:], arcCRC(buf[:]))
}

// FFmpeg 写的 Info 帧在单声道与 LSF 布局的低码率下可能不足 190 字节,例如 44.1 kHz
// 单声道 56 kbps 的 182 字节帧;按 FFmpeg 约定计算的 CRC 有效,gapless 信息照常采用。
func TestInfoTagCRCFFmpegShortFrame(t *testing.T) {
	data := readCorpus(t, "sine440-mono-44k1-cbr96.mp3") // FFmpeg 写入,Info 帧为 96 kbps
	p, _, lo := infoFrame(t, data)
	h, err := parseFrameHeader([4]byte(data[p : p+4]))
	if err != nil {
		t.Fatal(err)
	}
	short := append([]byte(nil), data[p:p+182]...)
	short[2] = short[2]&0x0D | 4<<4 // 码率序号 4(56 kbps),不加 padding
	if sh, err := parseFrameHeader([4]byte(short[:4])); err != nil || sh.frameSize != 182 {
		t.Fatalf("short Info frame header: frameSize %d, err %v", sh.frameSize, err)
	}
	setFFmpegTagCRC(short, lo)
	stream := append(append(append([]byte(nil), data[:p]...), short...), data[p+h.frameSize:]...)
	d, _ := decodeAll(t, bytes.NewReader(stream))
	if !d.Trimmed() || d.Frames() != 220500 {
		t.Fatalf("Trimmed()=%v Frames()=%d, want true and 220500", d.Trimmed(), d.Frames())
	}
}

// 标签 CRC 不符时 Info 帧内容(Xing 帧数、LAME gapless 字段)不可信:不裁剪、
// 不报告已裁剪,也不据其帧数判定截断,按无标签流处理。
func TestCorruptInfoTagIgnored(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(frame []byte, xo, lameOff int)
	}{
		{"enc delay bit flip", func(f []byte, xo, lo int) { f[lo+21] ^= 0x10 }},
		{"xing frame count", func(f []byte, xo, lo int) { binary.BigEndian.PutUint32(f[xo+8:], 500) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3") // Info 声明 193 帧
			p, xo, lo := infoFrame(t, data)
			tc.mutate(data[p:], xo, lo)
			d, _ := decodeAll(t, bytes.NewReader(data))
			if d.Trimmed() {
				t.Fatal("Trimmed()=true with a corrupt Info tag")
			}
			if d.Frames() != 193*1152 || d.DecodedFrames() != 193*1152 {
				t.Fatalf("Frames()=%d DecodedFrames()=%d, want both %d (untrimmed)", d.Frames(), d.DecodedFrames(), 193*1152)
			}
		})
	}
}

// 填充超出头裁之后的长度:宽松模式不做尾裁,记下与严格模式相同的异常。
func TestGaplessPaddingExceedsDecodedLenient(t *testing.T) {
	data := readCorpus(t, "short-tone-stereo-44k1-cbr192.mp3")
	p, _, lo := infoFrame(t, data)
	f := data[p:]
	f[lo+22] |= 0x0F
	f[lo+23] = 0xFF
	setLAMETagCRC(f, lo)
	d, pcm, err := decodeMode(t, bytes.NewReader(data), lenient())
	if err != nil {
		t.Fatal(err)
	}
	if want := (3*1152 - 1105) * 2; len(pcm) != want {
		t.Fatalf("delivered %d samples, want %d", len(pcm), want)
	}
	if r := d.Report(); r.First == nil || r.First.Kind != IssueCorrupt || r.First.Frame != -1 {
		t.Fatalf("first issue %+v", r.First)
	}
}

// 裁剪区间与一帧的相对位置:区间在帧外、覆盖帧首、帧尾、帧中间或整帧,余下的采样帧
// 保持顺序且声道不错位;立体声为主,单声道只留一例复制源与目标都依赖声道步长的区间。
func TestCutFrames(t *testing.T) {
	frame := func(n, nch int) []float64 { // n 个采样帧,第 i 帧为 (nch·i, …, nch·i+nch−1)
		s := make([]float64, nch*n)
		for i := range s {
			s[i] = float64(i)
		}
		return s
	}
	cases := []struct {
		name          string
		nch           int
		pos, from, to int64
		want          []float64
	}{
		{"stereo/before the frame", 2, 10, 0, 10, frame(4, 2)},
		{"stereo/after the frame", 2, 10, 14, 20, frame(4, 2)},
		{"stereo/empty window", 2, 10, 12, 12, frame(4, 2)},
		{"stereo/covers the start", 2, 10, 0, 12, []float64{4, 5, 6, 7}},
		{"stereo/covers the end", 2, 10, 12, 20, []float64{0, 1, 2, 3}},
		{"stereo/in the middle", 2, 10, 11, 13, []float64{0, 1, 6, 7}},
		{"stereo/covers the frame", 2, 10, 8, 20, []float64{}},
		{"mono/in the middle", 1, 10, 11, 13, []float64{0, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := frame(4, tc.nch)
			if got := cutFrames(s, tc.pos, tc.from, tc.to, tc.nch); !slices.Equal(got, tc.want) {
				t.Fatalf("cutFrames(pos %d, [%d, %d)) = %v, want %v", tc.pos, tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// Xing 帧数未知:帧数为 0,或头里根本没有帧数字段(flags 的 bit 0 清零,其后的字段前移)。
// 两种情形下两种模式都不做尾裁、不核对帧数,只做头裁,报告没有声明。
func TestXingFrameCountUnknown(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, data []byte, p, xo, lo int)
	}{
		{"zero count", func(t *testing.T, data []byte, p, xo, lo int) {
			f := data[p:]
			binary.BigEndian.PutUint32(f[xo+8:], 0)
			setLAMETagCRC(f, lo)
		}},
		{"no frames field", func(t *testing.T, data []byte, p, xo, lo int) {
			h, err := parseFrameHeader([4]byte(data[p : p+4]))
			if err != nil {
				t.Fatal(err)
			}
			f := data[p : p+h.frameSize]
			f[xo+7] &^= 1             // flags 低字节的 bit 0 标记帧数字段
			copy(f[xo+8:], f[xo+12:]) // 删去 4 字节帧数,其后的字段前移
			clear(f[len(f)-4:])
			setLAMETagCRC(f, lo-4) // LAME 扩展随之前移 4 字节
			if x, off, err := parseXing(f[xo:]); err != nil || x.frames != nil || xo+off != lo-4 {
				t.Fatalf("constructed Xing header: %+v, LAME offset %d (want %d), err=%v", x, xo+off, lo-4, err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
			p, xo, lo := infoFrame(t, data)
			tc.mutate(t, data, p, xo, lo)
			for _, m := range bothModes {
				t.Run(m.name, func(t *testing.T) {
					d, pcm, err := decodeMode(t, bytes.NewReader(data), m.opts...)
					if err != nil {
						t.Fatal(err)
					}
					if want := (193*1152 - 1105) * 2; len(pcm) != want || d.Frames() != 193*1152-1105 || !d.Trimmed() {
						t.Fatalf("delivered %d samples (Frames()=%d, trimmed %v), want %d", len(pcm), d.Frames(), d.Trimmed(), want)
					}
					if r := d.Report(); !r.Clean() || r.DeclaredFrames != -1 {
						t.Fatalf("report %+v", r)
					}
				})
			}
		})
	}
}

// 头裁与尾裁的区间都跨越帧边界:头裁 2529 个采样帧落在第 0–2 帧,声明的尾裁区间
// [221065, 222336) 横跨第 191、192 帧。两种模式的输出都是只做头裁的输出去掉末尾
// 1271 个采样帧。
func TestGaplessWindowsAcrossFrameBoundaries(t *testing.T) {
	const (
		frames  = 193
		delay   = 2000
		padding = 1800
		head    = delay + decoderDelay
		tail    = padding - decoderDelay
		decoded = frames * 1152
		kept    = decoded - head - tail
	)
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	p, xo, lo := infoFrame(t, data)
	f := data[p:]
	f[lo+21], f[lo+22], f[lo+23] = delay>>4, delay&0x0F<<4|padding>>8, padding&0xFF
	setLAMETagCRC(f, lo)

	// 只做头裁的基准:声明的帧数置 0(未知)时没有尾裁
	headOnly := bytes.Clone(data)
	g := headOnly[p:]
	binary.BigEndian.PutUint32(g[xo+8:], 0)
	setLAMETagCRC(g, lo)
	d, base, err := decodeMode(t, bytes.NewReader(headOnly), Strict())
	if err != nil {
		t.Fatal(err)
	}
	if want := (decoded - head) * 2; len(base) != want || d.Frames() != decoded-head {
		t.Fatalf("head-only baseline: %d samples, Frames()=%d, want %d samples", len(base), d.Frames(), want)
	}
	// 基准本身要与未裁剪的输出核对:标签 CRC 不符时不裁剪
	untrimmed := bytes.Clone(data)
	untrimmed[p+lo+34] ^= 1
	if _, raw, err := decodeMode(t, bytes.NewReader(untrimmed), Strict()); err != nil || !slices.Equal(base, raw[head*2:]) {
		t.Fatalf("head-only baseline differs from the untrimmed output past the head window (err=%v)", err)
	}

	for _, m := range bothModes {
		t.Run(m.name, func(t *testing.T) {
			d, pcm, err := decodeMode(t, bytes.NewReader(data), m.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if d.Frames() != kept || d.DecodedFrames() != decoded || !d.Trimmed() {
				t.Fatalf("Frames()=%d DecodedFrames()=%d Trimmed()=%v, want %d, %d, true", d.Frames(), d.DecodedFrames(), d.Trimmed(), kept, decoded)
			}
			if !slices.Equal(pcm, base[:len(base)-tail*2]) {
				t.Fatalf("output (%d samples) is not the head-only output minus its last %d sample frames", len(pcm), tail)
			}
			if r := d.Report(); !r.Clean() || r.DeclaredFrames != frames {
				t.Fatalf("report %+v", r)
			}
		})
	}
}

// 宽松模式丢弃的帧不产出 PCM。解码时丢弃的帧(side info 非法)帧尾已经确认,仍占它在
// 流中的位置:尾裁照常裁掉填充,输出的结尾与完好的流对齐,长度与 FFmpeg 相同。分帧层
// 丢弃的帧(帧内丢了字节)不计位置,填充落在输出之外不裁,长度与 mpg123 相同。
func TestTailTrimAfterDroppedFrames(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	starts, _ := audioFrames(t, data)
	_, want, err := decodeMode(t, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	badSideInfo := bytes.Clone(data)
	setBits(badSideInfo[starts[100]+4:], 32, 9, 511) // 第 0 granule 左声道的 big_values 超过 288
	lostBytes := slices.Delete(bytes.Clone(data), starts[100]+200, starts[100]+300)
	for _, tc := range []struct {
		name    string
		data    []byte
		frames  int64 // 交付的采样帧数:FFmpeg 与 mpg123 实测值
		aligned bool  // 输出的结尾与完好的流相同
	}{
		{"invalid side info", badSideInfo, 220500 - 1152, true},
		{"bytes lost in a frame", lostBytes, 220500 - 1152 + 731, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, got, err := decodeMode(t, bytes.NewReader(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if r := d.Report(); r.DroppedFrames != 1 {
				t.Fatalf("dropped %d frames, want 1", r.DroppedFrames)
			}
			if d.Frames() != tc.frames {
				t.Fatalf("Frames()=%d, want %d", d.Frames(), tc.frames)
			}
			tail := 2 * 1152 * 50
			if aligned := slices.Equal(got[len(got)-tail:], want[len(want)-tail:]); aligned != tc.aligned {
				t.Fatalf("end of the output aligned with the intact stream: %v, want %v", aligned, tc.aligned)
			}
			// 分帧层的帧序号与解码器一致:丢弃的帧不算已解码的帧
			if d.fr.frames != d.framesDecoded {
				t.Fatalf("framer counts %d frames, decoder %d", d.fr.frames, d.framesDecoded)
			}
		})
	}
}
