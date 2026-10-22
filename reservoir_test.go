package gl3d

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
)

func TestReservoirAssemble(t *testing.T) {
	r := newReservoir(511)

	// 帧 1:自带 4 字节 main data,main_data_begin=0
	f1, missing := r.assemble(0, []byte("AAAA"))
	if string(f1) != "AAAA" || missing != 0 {
		t.Fatalf("f1=%q missing=%d", f1, missing)
	}
	r.advance([]byte("AAAA"))

	// 帧 2:main_data_begin=2 → 数据从帧 1 的末尾 2 字节开始,即 "AA"+"BBBB"
	f2, missing := r.assemble(2, []byte("BBBB"))
	if string(f2) != "AABBBB" || missing != 0 {
		t.Fatalf("f2=%q missing=%d, want AABBBB and 0", f2, missing)
	}
	r.advance([]byte("BBBB"))

	// 帧 3:回指 8 字节(帧 1 与帧 2 的全部)
	f3, missing := r.assemble(8, []byte("CC"))
	if string(f3) != "AAAABBBBCC" || missing != 0 {
		t.Fatalf("f3=%q missing=%d, want AAAABBBBCC and 0", f3, missing)
	}
}

// main_data_begin 超出已收到的数据(码流从中途开始)时,缺失的前缀补 0,
// 视图中各字节的偏移与完整码流一致。
func TestReservoirMissingHistory(t *testing.T) {
	r := newReservoir(511)
	r.advance([]byte("AAAA"))
	w, missing := r.assemble(6, []byte("BB"))
	if string(w) != "\x00\x00AAAABB" || missing != 2 {
		t.Fatalf("window=%q missing=%d, want %q and 2", w, missing, "\x00\x00AAAABB")
	}
}

func TestReservoirCap(t *testing.T) {
	r := newReservoir(255) // MPEG-2/2.5 的 reservoir 上限(main_data_begin 为 8 位)
	r.advance(bytes.Repeat([]byte{1}, 300))
	if r.len() != 255 {
		t.Fatalf("len=%d, want 255(封顶)", r.len())
	}
	// 封顶后 main_data_begin 的最大值 255 仍完全落在保留的数据内
	if w, missing := r.assemble(255, []byte{2}); len(w) != 256 || missing != 0 {
		t.Fatalf("len(window)=%d missing=%d, want 256 and 0", len(w), missing)
	}
}

// 码流可以从中途开始(例如不重编码的剪切),开头几帧的 main_data_begin 指向流开始之前
// 的数据。数据起点早于流内第一个 main data 字节的 granule 输出静音;MPEG-1 第二个
// granule 置了 scfsi、而第一个 granule 缺数据时同样静音;其余照常解码,输出长度不变。
// 静音之后再过两个 granule,重叠与合成状态已与从头解码一致,输出逐位相同。
func TestStreamStartingMidReservoir(t *testing.T) {
	cases := []struct {
		file   string
		cut    int // 码流从第 cut 个音频帧开始
		silent int // 开头静音的 granule 数
	}{
		{"pluck-stereo-44k1-abr128-lame.mp3", 9, 1}, // 只有 granule 0 缺数据
		{"pluck-stereo-44k1-abr128-lame.mp3", 3, 2}, // granule 1 经 scfsi 依赖 granule 0
		{"sine440-mono-44k1-cbr96.mp3", 8, 4},       // 缺数据延续到第二帧
		{"pluck-stereo-24k-cbr64-lame.mp3", 7, 2},   // MPEG-2:每帧一个 granule
		{"pluck-stereo-8k-cbr16-lame.mp3", 3, 1},    // MPEG-2.5
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s@%d", tc.file, tc.cut), func(t *testing.T) {
			data := readCorpus(t, tc.file)
			// 对照流也去掉标签与 Info 帧:两者都不做 gapless 裁剪,样本按帧对齐
			starts, end := audioFrames(t, data)
			_, whole := decodeAll(t, bytes.NewReader(data[starts[0]:end]))
			d, pcm := decodeAll(t, bytes.NewReader(data[starts[tc.cut]:end]))
			nch, spf := d.Channels(), d.header.samplesPerFrame
			if want := (len(starts) - tc.cut) * spf * nch; len(pcm) != want {
				t.Fatalf("%d samples, want %d", len(pcm), want)
			}
			g := 576 * nch
			for i, v := range pcm[:tc.silent*g] {
				if v != 0 {
					t.Fatalf("sample %d in granule %d = %g, want silence", i, i/g, v)
				}
			}
			if !slices.ContainsFunc(pcm[tc.silent*g:(tc.silent+1)*g], func(v float64) bool { return v != 0 }) {
				t.Fatalf("granule %d is silent, want it decoded", tc.silent)
			}
			ref := whole[tc.cut*spf*nch:]
			for i := (tc.silent + 2) * g; i < len(pcm); i++ {
				if pcm[i] != ref[i] {
					t.Fatalf("sample %d = %g, want %g as when decoding from the start", i, pcm[i], ref[i])
				}
			}
		})
	}
}

// 数据起点恰为流内第一个 main data 字节的 granule 数据完整,照常解码。
func TestGranuleStartingAtFirstMainDataByte(t *testing.T) {
	data := buildL3Stream(t, 3, 0, [][2][2]l3Gran{{{}, {{lines: []int{1, -1, 1, 0}}}}})
	// 单声道 side info:main_data_begin 回指 1 字节,granule 0 声明 8 位,
	// 恰好整个落在流开始之前;granule 1 从本帧 main data 的第一个字节开始
	si := data[4:]
	setBits(si, 0, 9, 1)
	setBits(si, 18, 12, 8)
	_, pcm := decodeAll(t, bytes.NewReader(data))
	nonzero := func(v float64) bool { return v != 0 }
	if len(pcm) != 1152 || slices.ContainsFunc(pcm[:576], nonzero) || !slices.ContainsFunc(pcm[576:], nonzero) {
		t.Fatalf("%d samples, granule 0 silent=%v, granule 1 silent=%v; want 1152, true, false",
			len(pcm), !slices.ContainsFunc(pcm[:576], nonzero), !slices.ContainsFunc(pcm[576:], nonzero))
	}
}
