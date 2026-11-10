package gl3d

// 构造码流:按 ISO 13818-3 LSF side information 语法写出 MPEG-2.5 8000 Hz、64 kbps、
// 单声道、无 CRC 的帧序列,每帧 576 字节,main_data_begin 为 0。谱值是由帧序号决定的
// ±1 伪随机序列,全部用 Huffman 表 1 编码(region 探针除外)。

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var updateMixed8k = flag.Bool("update-mixed8k", false, "rewrite the constructed 8 kHz mixed-block corpus files")

// g8 是一帧(LSF 单 granule、单声道)的块结构与量化谱值。
type g8 struct {
	switching bool
	blockType int
	mixed     bool
	lines     []int // 从第 0 条谱线起的量化值(−1/0/1),长度为偶数

	// region 探针(仅切换块):ts1 非零时 table_select[0]=1、[1]=ts1,
	// 编码端在第 split 条谱线处从表 1 换到表 ts1。零值:全部用表 1。
	ts1, split int

	// scalefactor:sfc 为 LSF scalefac_compress(须 < 400,无 intensity),
	// scf 为按传输顺序的值,个数为该块形 nr_of_sfb 之和;sbg 为 subblock_gain。
	sfc int
	scf []int
	sbg [3]int
}

// lsfSlen 是 scalefac_compress < 400 时四段 scalefactor 的位宽(ISO 13818-3 2.4.3.2)。
func lsfSlen(sfc int) [4]int {
	return [4]int{(sfc >> 4) / 5, (sfc >> 4) % 5, (sfc & 15) >> 2, sfc & 3}
}

// nrOfSfb0 是 ISO 13818-3 nr_of_sfb_block 表第 0 组(sfc < 400、无 intensity):
// 长块(含起始/结束块)、纯短块、混合块。
func nrOfSfb0(g g8) [4]int {
	switch {
	case !g.switching || g.blockType != 2:
		return [4]int{6, 5, 5, 5}
	case g.mixed:
		return [4]int{6, 9, 9, 9}
	default:
		return [4]int{9, 9, 9, 9}
	}
}

// writePairT 用 Huffman 表 tab 写入一对量化值(|值| ≤ 表的范围,无 linbits),
// 非零值再跟一位符号位,1 为负。
func writePairT(t *testing.T, w *bitWriter, tab []huffEntry, x, y int) {
	t.Helper()
	abs := func(v int) int { return max(v, -v) }
	for _, e := range tab {
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
	t.Fatalf("no code for (%d, %d)", x, y)
}

// global_gain 取 180,谱值幅度为 2^((180−210)/4) = 2^−7.5。
const gain8k = 180

// lsfFormat 是构造码流的帧头参数(单声道、无 CRC、无 padding)。
type lsfFormat struct {
	versionBits int // 0 = MPEG-2.5,2 = MPEG-2
	srIdx       int
	frameSize   int // 72·码率(kbps)/采样率(kHz) 取整
}

// 帧头一律用 bitrate_index 8(64 kbps)。
var fmt8000 = lsfFormat{0, 2, 576}

// buildLSFStream 按 f 的帧头构造 LSF Layer III 帧序列,帧尾补零。g.sfc 为 0 时
// scalefac_compress 为 0(四段 slen 全为 0,不传 scalefactor,preflag 为 0);
// 无 region 探针时谱值全部用 Huffman 表 1 编码,part2_3_length 恰为 scalefactor 与
// big_values 区位数之和,count1 区为空,region 边界因此不影响解码结果。
func buildLSFStream(t *testing.T, f lsfFormat, frames []g8) []byte {
	t.Helper()
	var out []byte
	for i, g := range frames {
		var si, md bitWriter
		si.write(0, 8) // main_data_begin
		si.write(0, 1) // private_bits(单声道 1 位)
		ts1, split := 1, len(g.lines)
		if g.switching && g.ts1 != 0 {
			ts1, split = g.ts1, g.split
		}
		if g.sfc != 0 {
			if g.sfc >= 400 {
				t.Fatalf("frame %d: scalefac_compress %d not supported", i, g.sfc)
			}
			cnt, slen := nrOfSfb0(g), lsfSlen(g.sfc)
			k := 0
			for p := 0; p < 4; p++ {
				for j := 0; j < cnt[p]; j++ {
					md.write(g.scf[k], slen[p])
					k++
				}
			}
			if k != len(g.scf) {
				t.Fatalf("frame %d: %d scalefactors given, %d transmitted", i, len(g.scf), k)
			}
		}
		for p := 0; p < len(g.lines); p += 2 {
			tab := huffBigTables[1]
			if p >= split {
				tab = huffBigTables[ts1]
			}
			writePairT(t, &md, tab, g.lines[p], g.lines[p+1])
		}
		si.write(md.nbits, 12)      // part2_3_length(scalefactor + Huffman)
		si.write(len(g.lines)/2, 9) // big_values
		si.write(gain8k, 8)         // global_gain
		si.write(g.sfc, 9)          // scalefac_compress
		if g.switching {
			si.write(1, 1) // window_switching_flag
			si.write(g.blockType, 2)
			mixed := 0
			if g.mixed {
				mixed = 1
			}
			si.write(mixed, 1)
			si.write(1, 5)   // table_select[0]
			si.write(ts1, 5) // table_select[1]
			for k := 0; k < 3; k++ {
				si.write(g.sbg[k], 3) // subblock_gain[k]
			}
		} else {
			si.write(0, 1)
			for k := 0; k < 3; k++ {
				si.write(1, 5) // table_select[k]
			}
			si.write(7, 4) // region0_count
			si.write(7, 3) // region1_count
		}
		si.write(0, 1) // scalefac_scale
		si.write(0, 1) // count1table_select
		if si.nbits != 72 {
			t.Fatalf("frame %d: side info is %d bits, want 72", i, si.nbits)
		}
		h := mkHeader(f.versionBits, 1, 1, 8, f.srIdx, 0, 3, 0, 0)
		frame := append(append(h[:], si.bytes...), md.bytes...)
		if len(frame) > f.frameSize {
			t.Fatalf("frame %d needs %d bytes, have %d", i, len(frame), f.frameSize)
		}
		out = append(out, frame...)
		out = append(out, make([]byte, f.frameSize-len(frame))...)
	}
	return out
}

// splitmix64 是确定性的 64 位整数混合函数,为谱值与 scalefactor 提供伪随机位。
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// pattern8k 返回第 frame 帧的 ±1 谱值(确定性伪随机,只取决于帧序号),
// [lo, hi) 之外置 0,长度截到 hi(偶数)。
func pattern8k(frame, lo, hi int) []int {
	v := make([]int, hi)
	for k := lo; k < hi; k++ {
		if splitmix64(uint64(frame)<<20|uint64(k))&1 == 0 {
			v[k] = 1
		} else {
			v[k] = -1
		}
	}
	return v
}

// family8k 描述一条构造码流:cycle 是 10 帧一循环的块代码("0" 长块,"2m" 为块类型 2
// 且 mixed_block_flag 置位,"2" 无该标志),重复 4 次共 40 帧;非零谱线限于 [lo, hi);
// scfIn 中的块代码带非零 scalefactor 与 subblock_gain。
type family8k struct {
	name   string
	cycle  []string
	lo, hi int
	scfIn  []string
}

// scalefac_compress 202 对应 slen (2,2,2,2);scalefactor 取值为伪随机的 0..3。
const scfCompress8k = 202

// sbg8k 是带 scalefactor 的块使用的 subblock_gain。
var sbg8k = [3]int{1, 0, 2}

// scfValues8k 返回第 frame 帧的 n 个 scalefactor,伪随机、只取决于帧序号。
func scfValues8k(frame, n int) []int {
	v := make([]int, n)
	for k := range v {
		v[k] = int(splitmix64(0x5cf<<40|uint64(frame)<<8|uint64(k)) & 3)
	}
	return v
}

// 入库的四个族:
//
//	mixed     块类型 2 混合块序列,谱线全部非零
//	mixedlow  同上,只有 0..71 条谱线非零(长块区与短块区的交界)
//	mixedscf  同 mixed,混合块带 scalefactor 与 subblock_gain
//	startstop 块类型 1/3 带 mixed_block_flag,没有块类型 2
var mixed8kFamilies = []family8k{
	{"mixed-mono-8k-cbr64-built", strings.Fields("0 0 1 2m 2m 2m 2m 2m 3 0"), 0, 576, nil},
	{"mixedlow-mono-8k-cbr64-built", strings.Fields("0 0 1 2m 2m 2m 2m 2m 3 0"), 0, 72, nil},
	{"mixedscf-mono-8k-cbr64-built", strings.Fields("0 0 1 2m 2m 2m 2m 2m 3 0"), 0, 576, []string{"2m"}},
	{"startstop-mono-8k-cbr64-built", strings.Fields("0 1m 3m 1m 3m 0 0 1m 3m 0"), 0, 576, nil},
}

func (f family8k) blocks() []string {
	var b []string
	for r := 0; r < 4; r++ {
		b = append(b, f.cycle...)
	}
	return b
}

func (f family8k) frames() []g8 {
	var out []g8
	for i, code := range f.blocks() {
		g := g8{lines: pattern8k(i, f.lo, f.hi)}
		if code != "0" {
			g.switching = true
			g.blockType = int(code[0] - '0')
			g.mixed = strings.HasSuffix(code, "m")
		}
		if slices.Contains(f.scfIn, code) {
			n := 0
			for _, c := range nrOfSfb0(g) {
				n += c
			}
			g.sfc, g.scf, g.sbg = scfCompress8k, scfValues8k(i, n), sbg8k
		}
		out = append(out, g)
	}
	return out
}

// 入库的 8 kHz 混合块码流由本文件的构造器生成;构造器与文件必须一致,参考 PCM 才有出处。
func TestMixed8kCorpusMatchesBuilder(t *testing.T) {
	for _, f := range mixed8kFamilies {
		t.Run(f.name, func(t *testing.T) {
			data := buildLSFStream(t, fmt8000, f.frames())
			path := filepath.Join("corpus/synthetic", f.name+".mp3")
			if *updateMixed8k {
				if err := os.WriteFile(path, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("%s differs from the builder output (%d vs %d bytes)", path, len(got), len(data))
			}
		})
	}
}

// probeClass 是一类 region0 探针:format 与 cycle 同 family8k,target 是做换表的块代码。
type probeClass struct {
	name   string
	format lsfFormat
	cycle  []string
	target []string
}

// probeFrames 返回探针码流的帧;split < 0 为基准(全用表 1)。探针与基准谱值相同,
// 只在 target 块中令 table_select[0]=1、table_select[1]=3,编码端在第 split 条谱线
// 换表;解码器的 region0 边界恰为 split 时,探针与基准的输出逐字节相同。
func probeFrames(pc probeClass, split int) []g8 {
	var out []g8
	for r := 0; r < 4; r++ {
		for k, code := range pc.cycle {
			i := r*len(pc.cycle) + k
			g := g8{lines: pattern8k(i, 0, 576)}
			if code != "0" {
				g.switching = true
				g.blockType = int(code[0] - '0')
				g.mixed = strings.HasSuffix(code, "m")
				if split >= 0 && slices.Contains(pc.target, code) {
					g.ts1, g.split = 3, split
				}
			}
			out = append(out, g)
		}
	}
	return out
}

// 8 kHz 混合块的 region0 止于长块区上端(72),与 FFmpeg 相同;纯短块与起始/结束块
// 由解析隐含的 region0_count 决定。其余采样率的 36 见 TestRegionBoundsSwitched。
func TestRegion0Boundary(t *testing.T) {
	for _, tc := range []struct {
		pc probeClass
		p  int
	}{
		{probeClass{"8k_2m", fmt8000, strings.Fields("0 0 1 2m 2m 2m 2m 2m 3 0"), []string{"2m"}}, 72},
		{probeClass{"8k_2", fmt8000, strings.Fields("0 0 1 2 2 2 2 2 3 0"), []string{"2"}}, 72},
		{probeClass{"8k_13m", fmt8000, strings.Fields("0 1m 3m 1m 3m 0 0 1m 3m 0"), []string{"1m", "3m"}}, 108},
	} {
		t.Run(tc.pc.name, func(t *testing.T) {
			_, base, err := decodeMode(t, bytes.NewReader(buildLSFStream(t, tc.pc.format, probeFrames(tc.pc, -1))))
			if err != nil {
				t.Fatal("base:", err)
			}
			_, probe, err := decodeMode(t, bytes.NewReader(buildLSFStream(t, tc.pc.format, probeFrames(tc.pc, tc.p))))
			if err != nil {
				t.Fatalf("switch at line %d: %v", tc.p, err)
			}
			if !slices.Equal(probe, base) {
				t.Fatalf("switch at line %d does not reproduce the base output", tc.p)
			}
		})
	}
}
