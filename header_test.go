package gl3d

import (
	"errors"
	"testing"
)

func mkHeader(versionBits, layerBits, prot, bitrateIdx, srIdx, padding, chanMode, modeExt, emphasis int) [4]byte {
	return [4]byte{
		0xFF,
		0xE0 | byte(versionBits<<3) | byte(layerBits<<1) | byte(prot),
		byte(bitrateIdx<<4) | byte(srIdx<<2) | byte(padding<<1),
		byte(chanMode<<6) | byte(modeExt<<4) | byte(emphasis),
	}
}

func TestParseFrameHeaderValid(t *testing.T) {
	cases := []struct {
		name                             string
		h                                [4]byte
		version                          Version
		kbps, rate, frameSize, side, spf int
		channels                         int
		crc                              bool
	}{
		{"mpeg1 stereo 192k 44.1k", mkHeader(3, 1, 1, 11, 0, 0, 0, 0, 0), MPEG1, 192, 44100, 626, 32, 1152, 2, false},
		{"mpeg1 mono 96k 44.1k pad", mkHeader(3, 1, 1, 7, 0, 1, 3, 0, 0), MPEG1, 96, 44100, 314, 17, 1152, 1, false},
		{"mpeg1 stereo 320k 48k", mkHeader(3, 1, 1, 14, 1, 0, 0, 0, 0), MPEG1, 320, 48000, 960, 32, 1152, 2, false},
		{"mpeg1 stereo 128k 32k", mkHeader(3, 1, 1, 9, 2, 0, 0, 0, 0), MPEG1, 128, 32000, 576, 32, 1152, 2, false},
		{"mpeg2 stereo 64k 22.05k", mkHeader(2, 1, 1, 8, 0, 0, 0, 0, 0), MPEG2, 64, 22050, 208, 17, 576, 2, false},
		{"mpeg2 mono 24k 16k", mkHeader(2, 1, 1, 3, 2, 0, 3, 0, 0), MPEG2, 24, 16000, 108, 9, 576, 1, false},
		{"mpeg25 mono 32k 11.025k", mkHeader(0, 1, 1, 4, 0, 0, 3, 0, 0), MPEG25, 32, 11025, 208, 9, 576, 1, false},
		{"mpeg25 stereo 8k 8k", mkHeader(0, 1, 1, 1, 2, 0, 0, 0, 0), MPEG25, 8, 8000, 72, 17, 576, 2, false},
		{"mpeg1 with crc", mkHeader(3, 1, 0, 9, 0, 0, 0, 0, 0), MPEG1, 128, 44100, 417, 32, 1152, 2, true},
		{"joint stereo keeps modeExt", mkHeader(3, 1, 1, 11, 0, 0, 1, 2, 0), MPEG1, 192, 44100, 626, 32, 1152, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := parseFrameHeader(tc.h)
			if err != nil {
				t.Fatal(err)
			}
			if h.version != tc.version {
				t.Fatalf("version=%v, want %v", h.version, tc.version)
			}
			if h.bitrateKbps != tc.kbps || h.sampleRate != tc.rate {
				t.Fatalf("bitrate=%d rate=%d, want %d/%d", h.bitrateKbps, h.sampleRate, tc.kbps, tc.rate)
			}
			if h.frameSize != tc.frameSize {
				t.Fatalf("frameSize=%d, want %d", h.frameSize, tc.frameSize)
			}
			if h.sideInfoSize != tc.side || h.samplesPerFrame != tc.spf {
				t.Fatalf("side=%d spf=%d, want %d/%d", h.sideInfoSize, h.samplesPerFrame, tc.side, tc.spf)
			}
			if h.channels() != tc.channels {
				t.Fatalf("channels=%d, want %d", h.channels(), tc.channels)
			}
			if h.hasCRC != tc.crc {
				t.Fatalf("hasCRC=%v, want %v", h.hasCRC, tc.crc)
			}
		})
	}
}

func TestParseFrameHeaderModeExt(t *testing.T) {
	h, err := parseFrameHeader(mkHeader(3, 1, 1, 11, 0, 0, 1, 2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if h.channelMode != modeJointStereo || h.modeExt != 2 {
		t.Fatalf("mode=%v ext=%d", h.channelMode, h.modeExt)
	}
}

func TestParseFrameHeaderErrors(t *testing.T) {
	asUnsupported := func(err error) bool { var e *UnsupportedError; return errors.As(err, &e) }
	asCorrupt := func(err error) bool { var e *CorruptError; return errors.As(err, &e) }
	cases := []struct {
		name string
		h    [4]byte
		want func(error) bool // nil 表示只要求报错,不限错误类别
	}{
		{"no sync", [4]byte{0x00, 0xFB, 0x90, 0x00}, nil},
		{"reserved version", mkHeader(1, 1, 1, 9, 0, 0, 0, 0, 0), nil},
		{"reserved layer", mkHeader(3, 0, 1, 9, 0, 0, 0, 0, 0), nil},
		{"layer II unsupported", mkHeader(3, 2, 1, 9, 0, 0, 0, 0, 0), asUnsupported},
		{"layer I unsupported", mkHeader(3, 3, 1, 9, 0, 0, 0, 0, 0), asUnsupported},
		{"free format unsupported", mkHeader(3, 1, 1, 0, 0, 0, 0, 0, 0), asUnsupported},
		{"bad bitrate index", mkHeader(3, 1, 1, 15, 0, 0, 0, 0, 0), nil},
		{"reserved sample rate", mkHeader(3, 1, 1, 9, 3, 0, 0, 0, 0), nil},
		{"emphasis unsupported", mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 1), asUnsupported},
		// 保留值一律按损坏数据归类,且判定先于"不支持"特性:同一帧头同时
		// 含保留值与不支持特性时,须报损坏而不是不支持
		{"layer I with bad bitrate index", mkHeader(3, 3, 1, 15, 0, 0, 0, 0, 0), asCorrupt},
		{"layer II with reserved sample rate", mkHeader(3, 2, 1, 9, 3, 0, 0, 0, 0), asCorrupt},
		{"free format with reserved sample rate", mkHeader(3, 1, 1, 0, 3, 0, 0, 0, 0), asCorrupt},
		{"reserved emphasis", mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 2), asCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFrameHeader(tc.h)
			if err == nil {
				t.Fatal("want error")
			}
			if tc.want != nil && !tc.want(err) {
				t.Fatalf("err %v (%T), wrong category", err, err)
			}
		})
	}
}

func TestCRC16(t *testing.T) {
	// MPEG 音频 CRC:多项式 0x8005、初值 0xFFFF、不反射。"123456789" 的校验
	// 值为 0xAEE7;常见的 0xFEE8 对应初值 0x0000 的同名变体,不是本格式。
	data := []byte("123456789")
	if got := crc16(data); got != 0xAEE7 {
		t.Fatalf("crc16 check value = %#04x, want 0xAEE7", got)
	}
	if got := crc16(nil); got != 0xFFFF {
		t.Fatalf("crc16(nil) = %#04x, want 0xFFFF (init value)", got)
	}
	// 真实帧向量:lame -p 编码文件的首帧,CRC 覆盖帧头第 16–31 bit 加 32 字节
	// side info,存储值为 0x600E(帧头 fffa9064;末尾 2 字节与 Info 标签重叠)。
	frameBody := []byte{
		0x90, 0x64,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x49, 0x6e,
	}
	if got := crc16(frameBody); got != 0x600E {
		t.Fatalf("crc16 real frame = %#04x, want 0x600E", got)
	}
}
