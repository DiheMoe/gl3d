package mp3info

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

func u32be(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

// header 构造 4 字节的 Layer III 帧头(无 CRC)。
func header(versionBits, bitrateIdx, srIdx, padding, chanMode int) []byte {
	return []byte{
		0xFF,
		0xE0 | byte(versionBits<<3) | 0x03, // layer=01(III),protection=1(无 CRC)
		byte(bitrateIdx<<4) | byte(srIdx<<2) | byte(padding<<1),
		byte(chanMode << 6),
	}
}

// xingBlock 构造带全部四个可选字段的 Xing/Info 块,后接给定编码器字符串与
// delay/padding 的 LAME 标签。
func xingBlock(magic string, frames, bytes uint32, encoder string, encDelay, encPadding uint16) []byte {
	b := []byte(magic)
	b = append(b, 0, 0, 0, 0x0F)
	b = append(b, u32be(frames)...)
	b = append(b, u32be(bytes)...)
	b = append(b, make([]byte, 100)...)
	b = append(b, u32be(0)...)
	lame := make([]byte, 36)
	copy(lame, encoder)
	lame[21] = byte(encDelay >> 4)
	lame[22] = byte(encDelay<<4) | byte(encPadding>>8)
	lame[23] = byte(encPadding)
	return append(b, lame...)
}

func TestParseXingInfoWithLAME(t *testing.T) {
	cases := []struct {
		name       string
		hdr        []byte
		sideInfo   int
		magic      string
		frames     uint32
		bytes      uint32
		encoder    string
		delay      uint16
		padding    uint16
		tail       int
		version    string
		sampleRate int
		channels   int
		mode       string
		bitrate    int
		frameSize  int
		kind       string
	}{
		{"MPEG-1 stereo", header(3, 11, 0, 0, 0), 32, "Info", 193, 121625, "Lavc63.1.", 576, 1260, 512,
			"1", 44100, 2, "stereo", 192, 626, "info"},
		{"MPEG-2.5 mono", header(0, 4, 0, 0, 3), 9, "Info", 98, 20732, "Lavc63.1.", 576, 1260, 64,
			"2.5", 11025, 1, "mono", 32, 208, "info"},
		// floor(72*64000/22050)=208 + 1 padding
		{"MPEG-2 joint stereo with padding", header(2, 8, 0, 1, 1), 17, "Xing", 194, 41000, "LAME3.100", 576, 1000, 64,
			"2", 22050, 2, "joint_stereo", 64, 209, "xing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var data []byte
			data = append(data, tc.hdr...)
			data = append(data, make([]byte, tc.sideInfo)...)
			data = append(data, xingBlock(tc.magic, tc.frames, tc.bytes, tc.encoder, tc.delay, tc.padding)...)
			data = append(data, make([]byte, tc.tail)...) // 帧的其余部分

			info, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if info.MPEGVersion != tc.version || info.SampleRate != tc.sampleRate || info.Channels != tc.channels || info.ChannelMode != tc.mode {
				t.Fatalf("format: %+v", info)
			}
			if info.BitrateKbps != tc.bitrate || info.FrameSize != tc.frameSize {
				t.Fatalf("bitrate=%d frameSize=%d, want %d/%d", info.BitrateKbps, info.FrameSize, tc.bitrate, tc.frameSize)
			}
			if info.Xing == nil || info.Xing.Kind != tc.kind {
				t.Fatalf("xing: %+v", info.Xing)
			}
			if info.Xing.Frames == nil || *info.Xing.Frames != tc.frames {
				t.Fatalf("xing frames: %+v", info.Xing.Frames)
			}
			if info.Xing.Bytes == nil || *info.Xing.Bytes != tc.bytes || !info.Xing.TOC {
				t.Fatalf("xing bytes/toc: %+v", info.Xing)
			}
			if info.LAME == nil || info.LAME.Encoder != tc.encoder || info.LAME.EncDelay != int(tc.delay) || info.LAME.EncPadding != int(tc.padding) {
				t.Fatalf("lame: %+v", info.LAME)
			}
			if info.ID3v2 != nil || info.ID3v1 || info.VBRI != nil {
				t.Fatalf("unexpected tags: %+v", info)
			}
		})
	}
}

func TestParseLAMEEncoderField(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"", false},
		{"         ", false},
		{"\x00LAME", false},
		{"LAME\x003.1", false},
		{"LAME\n3.1", false},
		{"LAME\xff3.1", false},
		{"LAME3.100", true},
		{"Lavc62.11", true},
		{"GOGO3.13", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := make([]byte, 36)
			copy(b, tc.name)
			l := parseLAME(b)
			if (l != nil) != tc.want {
				t.Fatalf("parseLAME(%q) = %+v, want present %v", tc.name, l, tc.want)
			}
			if l != nil && l.Encoder != tc.name {
				t.Fatalf("encoder %q, want %q", l.Encoder, tc.name)
			}
		})
	}
}

func TestCRC16ARC(t *testing.T) {
	if got := crc16ARC([]byte("123456789")); got != 0xBB3D {
		t.Fatalf("crc16ARC(\"123456789\") = %#04x, want 0xbb3d", got)
	}
}

// 标签 CRC 有两种覆盖约定,任一吻合即算相符。LAME 覆盖 CRC 字段之前的字节。FFmpeg 覆盖
// 帧首 190 字节,其中的 CRC 字段按 0 计,帧不足 190 字节时缺的部分也按 0 计,不取下一帧的字节。
func TestParseLAMETagCRC(t *testing.T) {
	lame := func(frame []byte, pos int) uint16 { return crc16ARC(frame[:pos]) }
	ffmpeg := func(frame []byte, pos int) uint16 {
		b := make([]byte, 190)
		copy(b, frame)
		b[pos], b[pos+1] = 0, 0
		return crc16ARC(b)
	}
	for _, tc := range []struct {
		name     string
		hdr      []byte
		sideInfo int
		size     int
		crc      func([]byte, int) uint16
	}{
		{"MPEG-1 stereo, LAME", header(3, 11, 0, 0, 0), 32, 626, lame},
		{"MPEG-2.5 mono, LAME", header(0, 4, 0, 0, 3), 9, 208, lame},
		{"MPEG-2.5 mono, FFmpeg", header(0, 4, 0, 0, 3), 9, 208, ffmpeg},
		{"MPEG-2 mono shorter than 190 bytes, FFmpeg", header(2, 7, 0, 0, 3), 9, 182, ffmpeg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(tc.hdr, make([]byte, tc.sideInfo)...)
			data = append(data, xingBlock("Info", 3, 1000, "LAME3.100", 576, 1000)...)
			data = append(data, make([]byte, tc.size-len(data))...)
			pos := 4 + tc.sideInfo + 120 + 34
			binary.BigEndian.PutUint16(data[pos:], tc.crc(data, pos))
			data = append(data, bytes.Repeat([]byte{0x55}, 256)...) // 下一帧的字节不在覆盖范围内
			info, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if info.FrameSize != tc.size || info.LAME == nil || !info.LAME.CRCValid {
				t.Fatalf("frame size %d (want %d), lame %+v, want a valid CRC", info.FrameSize, tc.size, info.LAME)
			}
			data[4] ^= 0xFF // side info 区的第一个字节,两种约定都覆盖
			if info, err = Parse(data); err != nil || info.LAME == nil || info.LAME.CRCValid {
				t.Fatalf("after corrupting a covered byte: lame %+v, err %v, want an invalid CRC", info.LAME, err)
			}
		})
	}
}

func TestParseID3v2SyncsafeSizeAndFooter(t *testing.T) {
	tag := []byte{'I', 'D', '3', 4, 0, 0x10, 0, 0, 0, 22} // v2.4、footer 标志、syncsafe 长度 22
	tag = append(tag, make([]byte, 22)...)
	tag = append(tag, make([]byte, 10)...) // footer
	var data []byte
	data = append(data, tag...)
	data = append(data, header(3, 11, 0, 0, 0)...)
	data = append(data, make([]byte, 622)...)

	info, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID3v2 == nil || info.ID3v2.Version != "2.4.0" || info.ID3v2.Size != 42 {
		t.Fatalf("id3v2: %+v", info.ID3v2)
	}
	if info.MPEGVersion != "1" {
		t.Fatalf("frame not found after tag: %+v", info)
	}
}

func TestParseID3v1(t *testing.T) {
	var data []byte
	data = append(data, header(3, 9, 0, 0, 0)...)
	data = append(data, make([]byte, 626)...)
	tail := make([]byte, 128)
	copy(tail, "TAG")
	data = append(data, tail...)

	info, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ID3v1 {
		t.Fatal("want ID3v1 detected")
	}
}

func TestParseVBRI(t *testing.T) {
	var data []byte
	data = append(data, header(3, 9, 0, 0, 0)...)
	data = append(data, make([]byte, 32)...) // VBRI 距帧起点 36 字节
	data = append(data, "VBRI"...)
	data = append(data, 0, 1)             // 版本
	data = append(data, 0, 0)             // 延迟
	data = append(data, 0, 50)            // 质量
	data = append(data, u32be(121625)...) // 字节数
	data = append(data, u32be(193)...)    // 帧数
	data = append(data, make([]byte, 512)...)

	info, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.VBRI == nil || info.VBRI.Frames != 193 || info.VBRI.Bytes != 121625 {
		t.Fatalf("vbri: %+v", info.VBRI)
	}
	if info.Xing != nil {
		t.Fatalf("xing must not misfire on VBRI frame: %+v", info.Xing)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":            nil,
		"no sync":          append(header(3, 9, 0, 0, 0), make([]byte, 100)...)[1:],
		"reserved version": append(header(1, 9, 0, 0, 0), make([]byte, 626)...),
		"not layer III":    append([]byte{0xFF, 0xE0 | 0x18 | 0x05, 0x90, 0x00}, make([]byte, 626)...), // layer=10(II)
		"free format":      append(header(3, 0, 0, 0, 0), make([]byte, 626)...),
		"bad bitrate":      append(header(3, 15, 0, 0, 0), make([]byte, 626)...),
		"truncated header": {0xFF, 0xFB},
	}
	for name, data := range cases {
		if _, err := Parse(data); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

func TestParseCorpusGolden(t *testing.T) {
	data, err := os.ReadFile("../../../corpus/synthetic/sine440-stereo-44k1-cbr192.mp3")
	if err != nil {
		t.Skip("corpus file not available:", err)
	}
	info, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.MPEGVersion != "1" || info.SampleRate != 44100 || info.Channels != 2 ||
		info.BitrateKbps != 192 || info.FrameSize != 626 {
		t.Fatalf("format: %+v", info)
	}
	if info.ID3v2 == nil || info.ID3v2.Version != "2.4.0" || info.ID3v2.Size != 44 {
		t.Fatalf("id3v2: %+v", info.ID3v2)
	}
	if info.Xing == nil || info.Xing.Kind != "info" || *info.Xing.Frames != 193 || *info.Xing.Bytes != 121625 {
		t.Fatalf("xing: %+v", info.Xing)
	}
	if info.LAME == nil || info.LAME.EncDelay != 576 || info.LAME.EncPadding != 1260 || !info.LAME.CRCValid {
		t.Fatalf("lame: %+v", info.LAME)
	}
	if info.ID3v1 {
		t.Fatal("unexpected ID3v1")
	}
}

// 首帧是 Info 标签帧时,标签帧帧头的声道模式可能与音频帧不同(FFmpeg 写出的
// Info 帧为 stereo,音频帧为 joint stereo)。Audio 描述第一个音频帧。
func TestParseFirstAudioFrameAfterInfo(t *testing.T) {
	var data []byte
	data = append(data, header(3, 11, 0, 0, 0)...) // Info 帧:MPEG-1 192k 44.1k stereo,626 B
	data = append(data, make([]byte, 32)...)
	data = append(data, xingBlock("Info", 2, 2000, "Lavc63.1.", 576, 1260)...)
	data = append(data, make([]byte, 626-len(data))...)
	data = append(data, header(3, 9, 0, 1, 1)...) // 音频帧:128k、padding、joint stereo
	data = append(data, make([]byte, 600)...)
	info, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.ChannelMode != "stereo" {
		t.Fatalf("first frame mode %q, want stereo", info.ChannelMode)
	}
	if info.Audio == nil || info.Audio.ChannelMode != "joint_stereo" || info.Audio.BitrateKbps != 128 || info.Audio.SampleRate != 44100 {
		t.Fatalf("Audio = %+v, want joint_stereo 128 kbps 44100 Hz", info.Audio)
	}
}

func TestParseAudioIsFirstFrameWithoutTag(t *testing.T) {
	data := append(header(2, 8, 1, 0, 3), make([]byte, 300)...) // MPEG-2 64k 24k mono,无标签帧
	info, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.Audio == nil || info.Audio.MPEGVersion != "2" || info.Audio.ChannelMode != "mono" || info.Audio.SampleRate != 24000 {
		t.Fatalf("Audio = %+v, want MPEG-2 mono 24000 Hz", info.Audio)
	}
}
