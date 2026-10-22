package gl3d

import (
	"bytes"
	"slices"
	"testing"
)

// l3Gran 是构造帧中一个 granule/声道的块结构、scalefactor 与量化谱值。
// 谱值限取 −1/0/1 并全部用 Huffman 表 1 编码,region 边界因此不影响构造的
// 码流,测试只需描述块结构。
type l3Gran struct {
	switching   bool // window_switching_flag
	blockType   int
	mixed       bool
	scfCompress int   // MPEG-1 scalefac_compress;0 表示不传 scalefactor
	scf         []int // 传输顺序:长块 21 个;短块 12 带 × 3 窗,带主序
	lines       []int // 从第 0 条谱线起的量化值,长度为偶数
}

// slenMPEG1 是 MPEG-1 scalefac_compress 到 (slen1, slen2) 位宽的映射
// (ISO 11172-3 §2.4.2.7)。
var slenMPEG1 = [16][2]int{
	{0, 0}, {0, 1}, {0, 2}, {0, 3}, {3, 0}, {1, 1}, {1, 2}, {1, 3},
	{2, 1}, {2, 2}, {2, 3}, {3, 1}, {3, 2}, {3, 3}, {4, 2}, {4, 3},
}

// buildL3Stream 按 ISO 11172-3 的 side information 语法构造 MPEG-1 Layer III
// 44.1 kHz、128 kbps、无 CRC 的帧序列。main_data_begin 取 0:各帧主数据自足,
// 不经过 reservoir;帧尾补零(附加数据)。mode 为帧头的 mode(3 为单声道),
// modeExt 为 mode_extension;frames[i][gr][ch]。
func buildL3Stream(t *testing.T, mode, modeExt int, frames [][2][2]l3Gran) []byte {
	t.Helper()
	nch := 2
	if mode == 3 {
		nch = 1
	}
	const frameSize = 417 // 144000·128/44100,无 padding
	var out []byte
	for _, f := range frames {
		var si, md bitWriter
		si.write(0, 9) // main_data_begin
		if nch == 1 {
			si.write(0, 5)
		} else {
			si.write(0, 3)
		}
		si.write(0, 4*nch) // scfsi
		for gr := 0; gr < 2; gr++ {
			for ch := 0; ch < nch; ch++ {
				g := f[gr][ch]
				start := md.nbits
				// scfsi 为 0,每个 granule 都传全部 scalefactor:长块带 0–10 用
				// slen1、11–20 用 slen2;短块带 0–5 用 slen1、6–11 用 slen2
				split := 11
				if g.switching && g.blockType == 2 {
					split = 18
				}
				for i, v := range g.scf {
					n := slenMPEG1[g.scfCompress][0]
					if i >= split {
						n = slenMPEG1[g.scfCompress][1]
					}
					md.write(v, n)
				}
				for p := 0; p < len(g.lines); p += 2 {
					writeTable1Pair(t, &md, g.lines[p], g.lines[p+1])
				}
				si.write(md.nbits-start, 12) // part2_3_length
				si.write(len(g.lines)/2, 9)  // big_values
				si.write(190, 8)             // global_gain
				si.write(g.scfCompress, 4)   // scalefac_compress
				if g.switching {
					si.write(1, 1)
					si.write(g.blockType, 2)
					mixed := 0
					if g.mixed {
						mixed = 1
					}
					si.write(mixed, 1)
					si.write(1, 5) // table_select[0]
					si.write(1, 5) // table_select[1]
					si.write(0, 9) // subblock_gain[0..2]
				} else {
					si.write(0, 1)
					for i := 0; i < 3; i++ {
						si.write(1, 5) // table_select[i]
					}
					si.write(7, 4) // region0_count
					si.write(7, 3) // region1_count
				}
				si.write(0, 3) // preflag、scalefac_scale、count1table_select
			}
		}
		h := mkHeader(3, 1, 1, 9, 0, 0, mode, modeExt, 0)
		frame := append(append(h[:], si.bytes...), md.bytes...)
		if len(frame) > frameSize {
			t.Fatalf("frame needs %d bytes, have %d", len(frame), frameSize)
		}
		out = append(out, frame...)
		out = append(out, make([]byte, frameSize-len(frame))...)
	}
	return out
}

// writeTable1Pair 写入一对量化值的表 1 码字;非零值再跟一位符号位,1 为负。
func writeTable1Pair(t *testing.T, w *bitWriter, x, y int) {
	t.Helper()
	abs := func(v int) int { return max(v, -v) }
	for _, e := range huffTable1 {
		if int(e.x) == abs(x) && int(e.y) == abs(y) {
			w.write(int(e.code), int(e.length))
			for _, v := range []int{x, y} {
				if v < 0 {
					w.write(1, 1)
				} else if v > 0 {
					w.write(0, 1)
				}
			}
			return
		}
	}
	t.Fatalf("no table 1 code for (%d, %d)", x, y)
}

// pattern 返回长度为 n 的 −1/0/1 序列;step 不同则序列不同,便于区分左右
// 声道与不同块。
func pattern(n, step int) []int {
	v := make([]int, n)
	for i := range v {
		v[i] = []int{1, -1, 0, 1, 1, 0, -1}[(i*step)%7]
	}
	return v
}

func decodePCM(t *testing.T, stream []byte) []float64 {
	t.Helper()
	res := decodeWith(t, bytes.NewReader(stream), 1152)
	if res.err != nil {
		t.Fatalf("decode: %v", res.err)
	}
	return res.pcm
}

// channel 取交错 PCM 中的一个声道。
func channel(pcm []float64, nch, ch int) []float64 {
	var v []float64
	for i := ch; i < len(pcm); i += nch {
		v = append(v, pcm[i])
	}
	return v
}

// mixed_block_flag 置位时,最低两个子带用普通窗(block_type 0)变换,其余 30 个
// 子带按 block_type 变换(ISO 11172-3 §2.4.2.7),起始块与结束块同样适用。
// 谱值只放在第 0–27 条谱线(子带 0 全部与子带 1 的前 10 条;第 28–35 条为 0,
// 抗混叠蝶形不会把能量带进子带 2),带 mixed 标志的起始/结束块因此必须与普通
// 长块逐位相同。
func TestMixedStartStopBlocksUseNormalWindowInLowSubbands(t *testing.T) {
	low := pattern(28, 1)
	normal := l3Gran{lines: low}
	silent := l3Gran{}
	stream := func(a, b l3Gran) []byte {
		return buildL3Stream(t, 3, 0, [][2][2]l3Gran{
			{{normal}, {a}},
			{{b}, {normal}},
			{{silent}, {silent}},
			{{silent}, {silent}},
		})
	}
	want := decodePCM(t, stream(normal, normal))
	if !slices.ContainsFunc(want, func(v float64) bool { return v != 0 }) {
		t.Fatal("reference stream decodes to silence; the fixture carries no signal")
	}
	start := l3Gran{switching: true, blockType: 1, mixed: true, lines: low}
	stop := l3Gran{switching: true, blockType: 3, mixed: true, lines: low}
	got := decodePCM(t, stream(start, stop))
	if !slices.Equal(got, want) {
		t.Error("mixed start/stop blocks differ from normal blocks in subbands 0-1")
	}

	// 对照:不带 mixed 标志时起始/结束窗作用于全部子带,输出必须与普通长块不同。
	plainStart := l3Gran{switching: true, blockType: 1, lines: low}
	plainStop := l3Gran{switching: true, blockType: 3, lines: low}
	if slices.Equal(decodePCM(t, stream(plainStart, plainStop)), want) {
		t.Error("start/stop blocks without mixed flag decode like normal blocks")
	}
}

// block_type、mixed_block_flag 按 [gr][ch] 逐声道传输,只有 MS 模式要求两声道
// 相同(ISO 11172-3 §2.4.3.4)。双声道帧中每个声道的输出只取决于它自己的
// side information 与主数据,与另一声道的块类型无关。
func TestDualChannelBlockTypesAreIndependent(t *testing.T) {
	long := l3Gran{lines: pattern(64, 2)}
	short := l3Gran{switching: true, blockType: 2, lines: pattern(64, 3)}
	silent := l3Gran{}
	stream := func(left, right l3Gran) []byte {
		return buildL3Stream(t, 2, 0, [][2][2]l3Gran{
			{{left, right}, {left, right}},
			{{silent, silent}, {silent, silent}},
			{{silent, silent}, {silent, silent}},
		})
	}
	longLong := decodePCM(t, stream(long, long))
	shortShort := decodePCM(t, stream(short, short))
	longShort := decodePCM(t, stream(long, short))
	shortLong := decodePCM(t, stream(short, long))
	if slices.Equal(channel(longLong, 2, 0), channel(shortShort, 2, 0)) {
		t.Fatal("long and short fixtures decode identically; the test cannot tell them apart")
	}
	for _, c := range []struct {
		name      string
		got, want []float64
	}{
		{"left long next to right short", channel(longShort, 2, 0), channel(longLong, 2, 0)},
		{"right short next to left long", channel(longShort, 2, 1), channel(shortShort, 2, 1)},
		{"left short next to right long", channel(shortLong, 2, 0), channel(shortShort, 2, 0)},
		{"right long next to left short", channel(shortLong, 2, 1), channel(longLong, 2, 1)},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: output depends on the other channel's block type", c.name)
		}
	}
}

// intensity 带按 L = Li·kl、R = Li·kr 解码(ISO 11172-3 §2.4.3.4),MS 只作用于
// intensity 界限以下的带,不给 intensity 带附加任何因子。右声道全零时所有带
// 都是 intensity 带;scalefactor 为 0 即位置 0:kl = 0、kr = 1,输出应与左声道
// 静音、右声道承载同一谱值的普通立体声逐位相同,无论 MS 是否同时置位。
// 谱值限于第 0–63 条谱线,不触及不传 scalefactor 的最高带。
func TestIntensityBandsIgnoreMSScaling(t *testing.T) {
	content := l3Gran{lines: pattern(64, 2)}
	silent := l3Gran{}
	stream := func(mode, modeExt int, left, right l3Gran) []byte {
		return buildL3Stream(t, mode, modeExt, [][2][2]l3Gran{
			{{left, right}, {left, right}},
			{{silent, silent}, {silent, silent}},
			{{silent, silent}, {silent, silent}},
		})
	}
	want := decodePCM(t, stream(0, 0, silent, content))
	for _, c := range []struct {
		name    string
		modeExt int
	}{{"intensity", 1}, {"intensity with MS", 3}} {
		if got := decodePCM(t, stream(1, c.modeExt, content, silent)); !slices.Equal(got, want) {
			t.Errorf("%s: position 0 is not a unit-gain pan to the right channel", c.name)
		}
	}
}

// lineMask 返回 v 的副本,只保留 keep(i) 为真的谱线。
func lineMask(v []int, keep func(i int) bool) []int {
	out := make([]int, len(v))
	for i, x := range v {
		if keep(i) {
			out[i] = x
		}
	}
	return out
}

// 最高的 scalefactor 带(长块第 21 带,短块第 12 带)不传 scalefactor,intensity
// 位置沿用下面一带(短块为同一窗口)的值,非法位置同样沿用,即不做 intensity
// (dist10、FFmpeg、mpg123 与 minimp3 在下面一带为 intensity 带时一致)。右声道
// 全零时各带都在 intensity 区:位置 0 把左声道谱值整体移到右声道,位置 7 保持
// 左右声道不变,输出可用对应的普通立体声帧逐位核对。44.1 kHz 长块第 20 带为
// 第 342–417 条谱线、第 21 带为 418–575;短块第 11 带窗口 1 为第 348–377 条、
// 第 12 带窗口 1 为 464–519 条(ISO 11172-3 Table 3-B.8)。
func TestIntensityTopBandInheritsPosition(t *testing.T) {
	content := pattern(576, 2)
	silent := l3Gran{}
	long := func(lines []int) l3Gran { return l3Gran{lines: lines} }
	short := func(lines []int) l3Gran { return l3Gran{switching: true, blockType: 2, lines: lines} }
	stream := func(mode, modeExt int, left, right l3Gran) []byte {
		return buildL3Stream(t, mode, modeExt, [][2][2]l3Gran{
			{{left, right}, {left, right}},
			{{silent, silent}, {silent, silent}},
			{{silent, silent}, {silent, silent}},
		})
	}
	longIllegal20 := make([]int, 21)
	longIllegal20[20] = 7
	shortIllegal11w1 := make([]int, 36)
	shortIllegal11w1[11*3+1] = 7
	inLR := func(i int) bool { return i >= 342 }
	inLRShort := func(i int) bool { return (i >= 348 && i < 378) || (i >= 464 && i < 520) }
	not := func(f func(int) bool) func(int) bool { return func(i int) bool { return !f(i) } }
	for _, c := range []struct {
		name                string
		right               l3Gran // 全零谱值,只带位置
		left                l3Gran
		wantLeft, wantRight l3Gran
	}{
		{"long, position 0 everywhere",
			long(nil), long(content),
			silent, long(content)},
		{"short, position 0 everywhere",
			short(nil), short(content),
			short(nil), short(content)},
		{"long, illegal position in band 20",
			l3Gran{scfCompress: 3, scf: longIllegal20}, long(content),
			long(lineMask(content, inLR)), long(lineMask(content, not(inLR)))},
		{"short, illegal position in band 11 window 1",
			l3Gran{switching: true, blockType: 2, scfCompress: 3, scf: shortIllegal11w1}, short(content),
			short(lineMask(content, inLRShort)), short(lineMask(content, not(inLRShort)))},
	} {
		want := decodePCM(t, stream(0, 0, c.wantLeft, c.wantRight))
		if got := decodePCM(t, stream(1, 1, c.left, c.right)); !slices.Equal(got, want) {
			t.Errorf("%s: top band does not inherit the position below it", c.name)
		}
	}
}

// 混合块的 intensity 界限:短块部分逐窗口确定;长块部分只在三个窗口的短块部分
// 全为零时才可能进入 intensity 区,界限是右声道最后一个非零长块带(dist10、
// FFmpeg)。位置均为 0,intensity 带的谱值整体移到右声道。44.1 kHz 混合块的长部
// 为第 0–35 条谱线(长块第 5 带为第 20–23 条);短块第 3、4、5 带的窗口 1 依次为
// 第 40–43、54–59、74–81 条(ISO 11172-3 Table 3-B.8)。
func TestIntensityBoundInMixedBlocks(t *testing.T) {
	content := pattern(576, 2)
	mixed := func(lines []int) l3Gran { return l3Gran{switching: true, blockType: 2, mixed: true, lines: lines} }
	silent := l3Gran{}
	stream := func(mode, modeExt int, left, right l3Gran) []byte {
		return buildL3Stream(t, mode, modeExt, [][2][2]l3Gran{
			{{left, right}, {left, right}},
			{{silent, silent}, {silent, silent}},
			{{silent, silent}, {silent, silent}},
		})
	}
	for _, c := range []struct {
		name    string
		nonzero int              // 右声道唯一的非零谱线
		lr      func(i int) bool // 按左右声道解码的谱线
	}{
		{"right channel ends in long band 5", 20,
			func(i int) bool { return i < 24 }},
		{"right channel ends in short band 5 window 1", 74,
			func(i int) bool { return i < 36 || (i >= 40 && i < 44) || (i >= 54 && i < 60) || (i >= 74 && i < 82) }},
	} {
		right := make([]int, 576)
		right[c.nonzero] = 1
		wantRight := lineMask(content, func(i int) bool { return !c.lr(i) })
		wantRight[c.nonzero] = 1
		want := decodePCM(t, stream(0, 0, mixed(lineMask(content, c.lr)), mixed(wantRight)))
		if got := decodePCM(t, stream(1, 1, mixed(content), mixed(right))); !slices.Equal(got, want) {
			t.Errorf("%s: wrong intensity bound", c.name)
		}
	}
}
