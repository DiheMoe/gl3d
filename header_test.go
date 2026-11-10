package gl3d

import "testing"

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
		version                          mpegVersion
		kbps, rate, frameSize, side, spf int
		channels                         int
		mode                             channelMode
		modeExt                          int
		crc                              bool
	}{
		{"mpeg1 stereo 192k 44.1k", mkHeader(3, 1, 1, 11, 0, 0, 0, 0, 0), mpeg1, 192, 44100, 626, 32, 1152, 2, modeStereo, 0, false},
		{"mpeg1 mono 96k 44.1k pad", mkHeader(3, 1, 1, 7, 0, 1, 3, 0, 0), mpeg1, 96, 44100, 314, 17, 1152, 1, modeMono, 0, false},
		{"mpeg1 stereo 320k 48k", mkHeader(3, 1, 1, 14, 1, 0, 0, 0, 0), mpeg1, 320, 48000, 960, 32, 1152, 2, modeStereo, 0, false},
		{"mpeg1 stereo 128k 32k", mkHeader(3, 1, 1, 9, 2, 0, 0, 0, 0), mpeg1, 128, 32000, 576, 32, 1152, 2, modeStereo, 0, false},
		{"mpeg2 stereo 64k 22.05k", mkHeader(2, 1, 1, 8, 0, 0, 0, 0, 0), mpeg2, 64, 22050, 208, 17, 576, 2, modeStereo, 0, false},
		{"mpeg2 mono 24k 16k", mkHeader(2, 1, 1, 3, 2, 0, 3, 0, 0), mpeg2, 24, 16000, 108, 9, 576, 1, modeMono, 0, false},
		{"mpeg25 mono 32k 11.025k", mkHeader(0, 1, 1, 4, 0, 0, 3, 0, 0), mpeg25, 32, 11025, 208, 9, 576, 1, modeMono, 0, false},
		{"mpeg25 stereo 8k 8k", mkHeader(0, 1, 1, 1, 2, 0, 0, 0, 0), mpeg25, 8, 8000, 72, 17, 576, 2, modeStereo, 0, false},
		{"mpeg1 with crc", mkHeader(3, 1, 0, 9, 0, 0, 0, 0, 0), mpeg1, 128, 44100, 417, 32, 1152, 2, modeStereo, 0, true},
		{"joint stereo keeps modeExt", mkHeader(3, 1, 1, 11, 0, 0, 1, 2, 0), mpeg1, 192, 44100, 626, 32, 1152, 2, modeJointStereo, 2, false},
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
			if h.channelMode != tc.mode || h.modeExt != tc.modeExt {
				t.Fatalf("mode=%v ext=%d, want %v/%d", h.channelMode, h.modeExt, tc.mode, tc.modeExt)
			}
			if h.hasCRC != tc.crc {
				t.Fatalf("hasCRC=%v, want %v", h.hasCRC, tc.crc)
			}
		})
	}
}

// plausibleLayer3 对同步字之后的每一种帧头取值都与 parseFrameHeader 是否成功一致:
// 它只是不分配内存的预检,不能拒绝可以解析的帧头,也不能放过解析不了的。
func TestPlausibleLayer3MatchesParse(t *testing.T) {
	for v := 0; v < 1<<21; v++ {
		b := [4]byte{0xFF, 0xE0 | byte(v>>16), byte(v >> 8), byte(v)}
		_, err := parseFrameHeader(b)
		if got := plausibleLayer3(b[:]); got != (err == nil) {
			t.Fatalf("%x: plausibleLayer3=%v, parse error %v", b, got, err)
		}
	}
	for _, b := range [][]byte{nil, {0xFF}, {0xFF, 0xFB, 0x90}, {0, 0, 0, 0}, {0x00, 0xFB, 0x90, 0x00}, {0xFE, 0xFB, 0x90, 0}, {0xFF, 0xDB, 0x90, 0}} {
		if plausibleLayer3(b) {
			t.Errorf("%x: plausibleLayer3 accepts bytes that are not a frame header", b)
		}
		// 缺同步字是唯一只有 parseFrameHeader 的同步检查能拦住的错误
		if len(b) == 4 {
			if _, err := parseFrameHeader([4]byte(b)); err == nil {
				t.Errorf("%x: parseFrameHeader accepts a header without sync", b)
			}
		}
	}
	if allocs := testing.AllocsPerRun(100, func() { plausibleLayer3([]byte{0xFF, 0xFF, 0xFF, 0xFF}) }); allocs != 0 {
		t.Errorf("plausibleLayer3 allocates %v times on a rejected header", allocs)
	}
}

// layerFrameSize 与帧头解析给出的 Layer III 帧长一致,并给出 Layer I/II 的帧长。
func TestLayerFrameSize(t *testing.T) {
	for _, v := range []int{3, 2, 0} {
		for br := 1; br < 15; br++ {
			for sr := 0; sr < 3; sr++ {
				for pad := 0; pad < 2; pad++ {
					b := mkHeader(v, 1, 1, br, sr, pad, 0, 0, 0)
					h, err := parseFrameHeader(b)
					if err != nil {
						t.Fatal(err)
					}
					if got := layerFrameSize(b); got != h.frameSize {
						t.Fatalf("%x: %d, want %d", b, got, h.frameSize)
					}
				}
			}
		}
	}
	for _, tc := range []struct {
		name string
		b    [4]byte
		want int
	}{
		{"MPEG-1 Layer II 192k 44.1k", mkHeader(3, 2, 1, 10, 0, 0, 0, 0, 0), 626},
		{"MPEG-1 Layer I 256k 44.1k", mkHeader(3, 3, 1, 8, 0, 0, 0, 0, 0), 276},
		{"MPEG-1 Layer I 32k 48k padded", mkHeader(3, 3, 1, 1, 1, 1, 0, 0, 0), 36},
		{"MPEG-2 Layer II 64k 24k", mkHeader(2, 2, 1, 8, 1, 0, 0, 0, 0), 384},
		// Layer I 表末项 448 kbps:12*1000*448/32000 = 168,×4 = 672
		{"MPEG-1 Layer I 448k 32k", mkHeader(3, 3, 1, 14, 2, 0, 0, 0, 0), 672},
		// Layer II 表末项 384 kbps:144*1000*384/32000 = 1728,加 padding = 1729
		{"MPEG-1 Layer II 384k 32k padded", mkHeader(3, 2, 1, 14, 2, 1, 0, 0, 0), 1729},
		// LSF 的 Layer I 表第 12 项 192 kbps,44100/2 = 22050 Hz:(12*1000*192/22050 + 1)*4 = (104 + 1)*4 = 420(104.49 取整)
		{"MPEG-2 Layer I 192k 22.05k padded", mkHeader(2, 3, 1, 12, 0, 1, 0, 0, 0), 420},
		// bitrateTableLSF 第 4 项 32 kbps,44100/4 = 11025 Hz:144*1000*32/11025 = 417(417.96),加 padding = 418
		{"MPEG-2.5 Layer II 32k 11.025k padded", mkHeader(0, 2, 1, 4, 0, 1, 0, 0, 0), 418},
		{"free format", mkHeader(3, 2, 1, 0, 0, 0, 0, 0, 0), 0},
		{"no sync", [4]byte{0, 0, 0, 0}, 0},
	} {
		if got := layerFrameSize(tc.b); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}
