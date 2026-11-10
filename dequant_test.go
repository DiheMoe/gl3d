package gl3d

import (
	"bytes"
	"errors"
	"math"
	"math/rand"
	"slices"
	"testing"
)

func TestSrSfbRow(t *testing.T) {
	cases := []struct {
		v     mpegVersion
		srIdx int
		want  int
	}{
		{mpeg1, 0, 5}, {mpeg1, 1, 6}, {mpeg1, 2, 7},
		{mpeg2, 0, 2}, {mpeg2, 1, 3}, {mpeg2, 2, 4},
		{mpeg25, 0, 0}, {mpeg25, 1, 0}, {mpeg25, 2, 1},
	}
	for _, tc := range cases {
		if got := srSfbRow(tc.v, tc.srIdx); got != tc.want {
			t.Fatalf("srSfbRow(%v,%d)=%d, want %d", tc.v, tc.srIdx, got, tc.want)
		}
	}
}

func TestSlenTable(t *testing.T) {
	// scalefac_compress 的 (slen1, slen2) 位宽表(ISO 11172-3 §2.4.2.7)
	want1 := [16]int{0, 0, 0, 0, 3, 1, 1, 1, 2, 2, 2, 3, 3, 3, 4, 4}
	want2 := [16]int{0, 1, 2, 3, 0, 1, 2, 3, 1, 2, 3, 1, 2, 3, 2, 3}
	for i := 0; i < 16; i++ {
		if scfSlen[0][i] != want1[i] || scfSlen[1][i] != want2[i] {
			t.Fatalf("slen[%d] = %d/%d, want %d/%d", i, scfSlen[0][i], scfSlen[1][i], want1[i], want2[i])
		}
	}
}

func TestReadScalefactorsMPEG1Long(t *testing.T) {
	// 长块,sfc=6 → slen1=1、slen2=2;分段 {6,5,5,5},无 scfsi
	var w bitWriter
	vals := []int{1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 2, 3, 0, 1, 2, 3, 0, 1, 2, 3}
	for i, v := range vals {
		if i < 11 {
			w.write(v, 1)
		} else {
			w.write(v, 2)
		}
	}
	gi := &granuleInfo{scalefacCompress: 6}
	scf := make([]int, 22)
	_, err := readScalefactors(&bitReader{data: w.bytes}, mpeg1, gi, [4]int{}, shapeLong, false, nil, false, scf, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vals {
		if scf[i] != v {
			t.Fatalf("scf[%d]=%d, want %d", i, scf[i], v)
		}
	}
}

func TestReadScalefactorsScfsiReuse(t *testing.T) {
	// granule 1 且 scfsi[0]=1:第 0 组(6 带)复用 granule 0,不读位;
	// 组 1 为 5 带 × 1 位,组 2、3 为 5 带 × 2 位(sfc=6)
	var w bitWriter
	for _, v := range []int{1, 0, 1, 0, 1} {
		w.write(v, 1)
	}
	for _, v := range []int{2, 3, 0, 1, 2, 3, 0, 1, 2, 3} {
		w.write(v, 2)
	}
	gi := &granuleInfo{scalefacCompress: 6}
	prev := []int{9, 9, 9, 9, 9, 9}
	scf := make([]int, 22)
	_, err := readScalefactors(&bitReader{data: w.bytes}, mpeg1, gi, [4]int{1, 0, 0, 0}, shapeLong, true, prev, false, scf, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if scf[i] != 9 {
			t.Fatalf("scf[%d]=%d, want 9 (reused)", i, scf[i])
		}
	}
	if scf[6] != 1 || scf[11] != 2 || scf[20] != 3 {
		t.Fatalf("scf[6]=%d scf[11]=%d scf[20]=%d, want 1/2/3", scf[6], scf[11], scf[20])
	}
}

func TestLSFScfSizes(t *testing.T) {
	// 混合基数展开;变体 = 消费行数 × 4(与 minimp3 的退出语义一致)。
	// sfc=0:消费行 0 后 sfc<0 → 变体 4,尺寸全 0
	sizes, variant, err := lsfScfSizes(0, false)
	if err != nil {
		t.Fatal(err)
	}
	if sizes != [4]int{0, 0, 0, 0} || variant != 4 {
		t.Fatalf("sfc=0: %v/%d, want [0 0 0 0]/4", sizes, variant)
	}
	// sfc=500 → 消费 3 行 → 变体 12;最终尺寸 {0,0,0,0}
	s2, v2, err := lsfScfSizes(500, false)
	if err != nil {
		t.Fatal(err)
	}
	if s2 != [4]int{0, 0, 0, 0} || v2 != 12 {
		t.Fatalf("sfc=500: %v/%d, want [0 0 0 0]/12", s2, v2)
	}
	// intensity:sfc=502 → sfc>>1=251,从变体 12 起 → 变体 24,尺寸 {2,1,0,0}
	s3, v3, err := lsfScfSizes(502, true)
	if err != nil {
		t.Fatal(err)
	}
	if s3 != [4]int{2, 1, 0, 0} || v3 != 24 {
		t.Fatalf("sfc=502 intensity: %v/%d, want [2 1 0 0]/24", s3, v3)
	}
}

// 反量化逐条谱线符合 ISO 11172-3 §2.4.3.4 的公式:覆盖长/短/混合块、
// subblock_gain、preflag(pretab)、scalefac_scale 与 linbits 范围的大值。
func TestDequantizeMatchesFormula(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, shape := range []blockShape{shapeLong, shapeShort, shapeMixed} {
		for trial := 0; trial < 20; trial++ {
			bm := bandMapFor(mpeg1, rng.Intn(3), shape)
			gi := &granuleInfo{
				globalGain:    100 + rng.Intn(120),
				scalefacScale: rng.Intn(2),
				subblockGain:  [3]int{rng.Intn(8), rng.Intn(8), rng.Intn(8)},
			}
			if shape == shapeLong {
				gi.preflag = rng.Intn(2)
			}
			scf := make([]int, bm.scfLen)
			for b := range scf {
				scf[b] = rng.Intn(16)
			}
			var is [576]int
			for i := range is {
				switch r := rng.Intn(10); {
				case r < 4:
				case r < 8:
					is[i] = rng.Intn(31) - 15
				default:
					is[i] = rng.Intn(16413) - 8206 // 15 + 13 位 linbits 的最大幅度
				}
			}
			var xr [576]float64
			for i := range xr {
				xr[i] = math.NaN() // 必须全部被覆写
			}
			dequantize(gi, bm, scf, &is, 576, &xr)
			for i, v := range is {
				b := bm.scfIdx[i]
				s := float64(scf[b])
				if gi.preflag != 0 && bm.long && b < 22 {
					s += float64(pretab[b])
				}
				e := float64(gi.globalGain - 210)
				if bm.short[i] {
					e -= 8 * float64(gi.subblockGain[bm.window[i]])
				}
				want := math.Pow(math.Abs(float64(v)), 4.0/3.0) * math.Pow(2, e/4) *
					math.Pow(2, -float64(gi.scalefacScale+1)/2*s)
				if v < 0 {
					want = -want
				}
				if d := math.Abs(xr[i] - want); d > 1e-12*math.Abs(want) {
					t.Fatalf("shape %d line %d is=%d: got %.17g, want %.17g", shape, i, v, xr[i], want)
				}
			}
		}
	}
}

func TestIntensityPanFormula(t *testing.T) {
	// pos 3 为正中间:kl = kr = 0.5。arm64 上编译器把 math.Tan 内的乘加融合为
	// FMA,tan(π/4) 末位可差 1 ulp,故这里用容差而不是逐位比较。
	kl, kr := intensityPan(3)
	if math.Abs(kl-0.5) > 1e-15 || math.Abs(kr-0.5) > 1e-15 {
		t.Fatalf("pos 3: (%.17g,%.17g), want 0.5", kl, kr)
	}
}

func TestMidsideStereo(t *testing.T) {
	l := []float64{1, -1, 0.5}
	r := []float64{1, 1, -0.5}
	msStereo(l, r)
	inv := 1 / math.Sqrt2
	if math.Abs(l[0]-math.Sqrt2) > 1e-12 || math.Abs(r[0]) > 1e-12 {
		t.Fatalf("l[0]=%g r[0]=%g, want √2/0", l[0], r[0])
	}
	if math.Abs(l[2]) > 1e-12 || math.Abs(r[2]-inv) > 1e-12 {
		t.Fatalf("l[2]=%g r[2]=%g, want 0/%g", l[2], r[2], inv)
	}
}

// big_values 表 0 不读取任何位。即使 576 条谱线都由表 0 给出,声明的
// part2_3_length 也不能越过物理窗口,否则是损坏,不能与允许丢弃的 count1
// stuffing 混为一谈。出错后频谱置零、读位置停在 granule 结尾,供宽松模式
// 容错后接着解码。
func TestDecodeSpectraEnforcesBitBudgets(t *testing.T) {
	h, _ := parseFrameHeader(mkHeader(3, 1, 1, 9, 0, 0, 3, 0, 0))
	bm := bandMapFor(mpeg1, 0, shapeLong)
	gi := granuleInfo{part2_3Length: 9, bigValues: 288}
	var si sideInfo
	si.gr[0][0] = gi
	br := &bitReader{data: []byte{0}}
	var gs granuleSpectrum
	for i := range gs.xr {
		gs.xr[i] = 1
	}
	err := decodeSpectra(&h, &si, 0, 0, br, bm, shapeLong, nil, false, &gs)
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("got %v, want CorruptError", err)
	}
	if br.pos != gi.part2_3Length {
		t.Fatalf("read position %d, want the granule end %d", br.pos, gi.part2_3Length)
	}
	if i := slices.IndexFunc(gs.xr[:], func(v float64) bool { return v != 0 }); i >= 0 {
		t.Fatalf("line %d = %g after the error, want a zeroed spectrum", i, gs.xr[i])
	}
}

// 该帧恰好在 count1 的不完整码字后结束。后面是否还能预读下一帧不应
// 改变 stuffing 的解释;两帧都只编码零谱线,应完整交付 2304 个静音样本。
func TestCount1StuffingAtPhysicalWindowEnd(t *testing.T) {
	data := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 3, 0, 0), 417, 2)
	for _, off := range []int{0, 417} {
		si := data[off+4 : off+21]
		setBits(si, 18, 12, 3167)
		setBits(si, 18+12, 9, 288)
		setBits(si, 18+59, 12, 1)
	}
	pcm := decodePCM(t, data)
	if len(pcm) != 2304 {
		t.Fatalf("decoded %d mono samples, want 2304", len(pcm))
	}
	for i, v := range pcm {
		if v != 0 {
			t.Fatalf("sample %d = %g, want silence", i, v)
		}
	}
}

// 检查增益查表所依赖的整个字段范围,覆盖最小短区指数 -390/4 与最大
// 指数 45/4。参照直接计算公式,不使用 gainQuarter 表。
func TestDequantizeGainExtremes(t *testing.T) {
	for sfc := 0; sfc < 512; sfc++ {
		for _, intensity := range []bool{false, true} {
			widths, _, err := lsfScfSizes(sfc, intensity)
			if err != nil {
				t.Fatal(err)
			}
			for _, width := range widths {
				if width > 5 {
					t.Fatalf("scalefactor width %d exceeds gain table's 31-value bound", width)
				}
			}
		}
	}
	for _, shape := range []blockShape{shapeLong, shapeShort, shapeMixed} {
		bm := bandMapFor(mpeg2, 0, shape)
		for _, gg := range []int{0, 255} {
			for _, scale := range []int{0, 1} {
				for _, sub := range []int{0, 7} {
					for _, scfVal := range []int{0, 31} {
						for _, pre := range []int{0, 1} {
							gi := granuleInfo{globalGain: gg, scalefacScale: scale, subblockGain: [3]int{sub, sub, sub}, preflag: pre}
							var scf [40]int
							var is [576]int
							var xr [576]float64
							for b := range scf {
								scf[b] = scfVal
							}
							for i := range is {
								is[i] = 1
							}
							dequantize(&gi, bm, scf[:], &is, 576, &xr)
							for i, got := range xr {
								b := bm.scfIdx[i]
								s := float64(scfVal)
								if pre != 0 && bm.long {
									s += float64(pretab[b])
								}
								e := float64(gg-210) / 4
								if bm.short[i] {
									e -= 2 * float64(sub)
								}
								want := math.Exp2(e - float64(scale+1)*s/2)
								if got != want {
									t.Fatalf("shape %d line %d: got %.17g, want %.17g", shape, i, got, want)
								}
							}
						}
					}
				}
			}
		}
	}
}

// 构造只含右声道第 96 条非零谱线的 LSF 流。intensity 版本在右声道
// 使用 compress=502,scalefactor 全零;普通 stereo 版本使用 compress=0。
// 右声道的非零频带不会执行 intensity pan,两条流的有效频谱应完全相同。
func lsfPreflagStream(t *testing.T, versionBits int, intensity bool) []byte {
	t.Helper()
	var si, md bitWriter
	si.write(0, 8+2)
	for ch := 0; ch < 2; ch++ {
		part, big, sfc := 0, 0, 0
		if ch == 1 {
			part, big = 51, 49
			if intensity {
				part, sfc = 75, 502
			}
		}
		si.write(part, 12)
		si.write(big, 9)
		si.write(190, 8)
		si.write(sfc, 9)
		si.write(0, 1)
		for i := 0; i < 3; i++ {
			si.write(1, 5)
		}
		si.write(7, 4)
		si.write(7, 3)
		si.write(0, 2)
	}
	if intensity {
		md.write(0, 24) // compress=502:8 个 2 位和 8 个 1 位 scalefactor
	}
	for p := 0; p < 48; p++ {
		writeTable1Pair(t, &md, 0, 0)
	}
	writeTable1Pair(t, &md, 1, 0)
	mode, ext := 0, 0
	if intensity {
		mode, ext = 1, 1
	}
	header := mkHeader(versionBits, 1, 1, 8, 0, 0, mode, ext, 0)
	h, err := parseFrameHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, h.frameSize)
	copy(frame, header[:])
	copy(frame[4:], si.bytes)
	copy(frame[4+len(si.bytes):], md.bytes)
	return bytes.Repeat(frame, 4)
}

func TestLSFIntensityDoesNotApplyNormalPreflag(t *testing.T) {
	for _, versionBits := range []int{2, 0} {
		plain := decodePCM(t, lsfPreflagStream(t, versionBits, false))
		intensity := decodePCM(t, lsfPreflagStream(t, versionBits, true))
		if !slices.Equal(intensity, plain) {
			t.Fatalf("MPEG version bits %d: intensity compress=502 changed the non-intensity frequency band's gain", versionBits)
		}
	}
}

// 混合块的长块区与长块表的前 nLong 带相同,其上与纯短块从第 3 带起的布局相同。
func TestMixedBandMaps(t *testing.T) {
	for v := mpeg1; v <= mpeg25; v++ {
		nLong := 8
		if v != mpeg1 {
			nLong = 6
		}
		for sr := 0; sr < 3; sr++ {
			long, short, mixed := bandMapFor(v, sr, shapeLong), bandMapFor(v, sr, shapeShort), bandMapFor(v, sr, shapeMixed)
			if !slices.Equal(mixed.sfbStart[:nLong+1], long.sfbStart[:nLong+1]) {
				t.Errorf("v%d sr%d: long part %v, long map %v", v, sr, mixed.sfbStart[:nLong+1], long.sfbStart[:nLong+1])
			}
			if !slices.Equal(mixed.sfbStart[nLong:], short.sfbStart[9:]) {
				t.Errorf("v%d sr%d: short part %v, short map from band 3 %v", v, sr, mixed.sfbStart[nLong:], short.sfbStart[9:])
			}
		}
	}
	bm := bandMapFor(mpeg25, 2, shapeMixed) // 8000 Hz
	if got := bm.shortBands[0].posA; got != 72 {
		t.Fatalf("8 kHz mixed short part starts at line %d, want 72", got)
	}
	if want := []int{0, 12, 24, 36, 48, 60, 72, 84, 96}; !slices.Equal(bm.sfbStart[:9], want) {
		t.Fatalf("8 kHz mixed band starts %v, want %v", bm.sfbStart[:9], want)
	}
}

func TestRegionBoundsSwitched(t *testing.T) {
	for _, tc := range []struct {
		v         mpegVersion
		sr        int
		blockType int
		mixed     bool
		want      int
	}{
		{mpeg25, 2, 2, true, 72},  // 8000 Hz
		{mpeg25, 0, 2, true, 36},  // 11025 Hz
		{mpeg25, 1, 2, true, 36},  // 12000 Hz
		{mpeg2, 0, 2, true, 36},   // 22050 Hz
		{mpeg2, 1, 2, true, 36},   // 24000 Hz
		{mpeg2, 2, 2, true, 36},   // 16000 Hz
		{mpeg1, 0, 2, true, 36},   // 44100 Hz
		{mpeg25, 2, 2, false, 72}, // 纯短块
		{mpeg2, 0, 2, false, 36},
		{mpeg25, 2, 1, true, 108}, // 起始块按长块排列
		{mpeg2, 0, 3, true, 54},
	} {
		gi := granuleInfo{windowSwitching: true, blockType: tc.blockType, mixedBlock: tc.mixed, regionCount: [2]int{7, 255}}
		if tc.blockType == 2 && !tc.mixed {
			gi.regionCount = [2]int{8, 255}
		}
		rb := regionBounds(&gi, bandMapFor(tc.v, tc.sr, gi.shape()))
		if rb[1] != tc.want || rb[2] != 576 {
			t.Errorf("v%d sr%d type %d mixed %v: bounds %v, want region0 end %d and region1 to 576", tc.v, tc.sr, tc.blockType, tc.mixed, rb, tc.want)
		}
	}
}
