package gl3d

import (
	"bytes"
	"encoding/binary"
	"errors"
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

// setLAMETagCRC 按 LAME 的约定重算 Info 标签 CRC:CRC-16/ARC(反射多项式
// 0xA001、初值 0),覆盖 CRC 字段之前的全部字节。
func setLAMETagCRC(frame []byte, lameOff int) {
	pos := lameOff + 34
	var c uint16
	for _, b := range frame[:pos] {
		c ^= uint16(b)
		for i := 0; i < 8; i++ {
			if c&1 != 0 {
				c = c>>1 ^ 0xA001
			} else {
				c >>= 1
			}
		}
	}
	binary.BigEndian.PutUint16(frame[pos:], c)
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

// CRC 有效、但声明的尾部填充超过可裁剪的解码输出时,gapless 信息自相矛盾:
// 报格式错误而不猜测裁剪量。
func TestGaplessPaddingExceedsDecoded(t *testing.T) {
	data := readCorpus(t, "short-tone-stereo-44k1-cbr192.mp3") // 3 帧,enc_delay 576
	p, _, lo := infoFrame(t, data)
	f := data[p:]
	f[lo+22] |= 0x0F // enc_padding = 0xFFF:仅其本身已远超头裁后剩余的 3×1152−(576+529)
	f[lo+23] = 0xFF
	setLAMETagCRC(f, lo)
	_, err := drain(t, bytes.NewReader(data))
	var c *CorruptError
	if !errors.As(err, &c) {
		t.Fatalf("err=%v, want CorruptError", err)
	}
}
