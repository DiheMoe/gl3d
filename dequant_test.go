package gl3d

import (
	"math"
	"math/rand"
	"testing"
)

func TestSfbBounds(t *testing.T) {
	// 44100 Hz 长块的带边界(ISO 11172-3 Table B.8)
	b := sfbBounds(sfbLongWidths[5][:]) // 行 5 = MPEG-1 44100
	want := []int{0, 4, 8, 12, 16, 20, 24, 30, 36, 44, 52, 62, 74, 90, 110, 134, 162, 196, 238, 288, 342, 418, 576}
	if len(b) != len(want) {
		t.Fatalf("len=%d, want %d", len(b), len(want))
	}
	for i := range want {
		if b[i] != want[i] {
			t.Fatalf("bound[%d]=%d, want %d", i, b[i], want[i])
		}
	}
}

func TestSrSfbRow(t *testing.T) {
	cases := []struct {
		v     Version
		srIdx int
		want  int
	}{
		{MPEG1, 0, 5}, {MPEG1, 1, 6}, {MPEG1, 2, 7},
		{MPEG2, 0, 2}, {MPEG2, 1, 3}, {MPEG2, 2, 4},
		{MPEG25, 0, 0}, {MPEG25, 1, 0}, {MPEG25, 2, 1},
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
	_, err := readScalefactors(&bitReader{data: w.bytes}, MPEG1, gi, [4]int{}, shapeLong, false, nil, false, scf, nil)
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
	_, err := readScalefactors(&bitReader{data: w.bytes}, MPEG1, gi, [4]int{1, 0, 0, 0}, shapeLong, true, prev, false, scf, nil)
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

func TestDequantizeKnownValues(t *testing.T) {
	// gg=210、scf=0 → xr = is^(4/3)
	gi := &granuleInfo{globalGain: 210, scalefacScale: 0}
	scf := make([]int, 22)
	bm := bandMapFor(MPEG1, 0, shapeLong) // 44100 长块
	var is [576]int
	is[0] = 8 // 8^(4/3) = 16
	is[4] = -1
	var xr [576]float64
	dequantize(gi, bm, scf, &is, 576, &xr)
	if math.Abs(xr[0]-16) > 1e-12 {
		t.Fatalf("xr[0]=%g, want 16", xr[0])
	}
	if math.Abs(xr[4]+1) > 1e-12 {
		t.Fatalf("xr[4]=%g, want -1", xr[4])
	}
	// scalefac_scale=1 且 scf=2 → 乘子 2^(-2*2/2) = 1/4
	gi.scalefacScale = 1
	scf2 := make([]int, 22)
	scf2[0] = 2
	var xr2 [576]float64
	dequantize(gi, bm, scf2, &is, 576, &xr2)
	if math.Abs(xr2[0]-4) > 1e-9 {
		t.Fatalf("xr2[0]=%g, want 4", xr2[0])
	}
}

// 反量化逐条谱线符合 ISO 11172-3 §2.4.3.4 的公式:覆盖长/短/混合块、
// subblock_gain、preflag(pretab)、scalefac_scale 与 linbits 范围的大值。
func TestDequantizeMatchesFormula(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, shape := range []blockShape{shapeLong, shapeShort, shapeMixed} {
		for trial := 0; trial < 20; trial++ {
			bm := bandMapFor(MPEG1, rng.Intn(3), shape)
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

// nz 之后的谱线必须清零:输出缓冲在 granule 间复用,残留值会混入下一 granule。
func TestDequantizeZeroesBeyondNZ(t *testing.T) {
	gi := &granuleInfo{globalGain: 210}
	bm := bandMapFor(MPEG1, 0, shapeLong)
	var is [576]int
	is[0], is[1] = 3, -2
	var xr [576]float64
	for i := range xr {
		xr[i] = 7
	}
	dequantize(gi, bm, make([]int, bm.scfLen), &is, 2, &xr)
	if xr[0] == 0 || xr[1] == 0 {
		t.Fatalf("xr[0:2]=%v, want non-zero", xr[:2])
	}
	for i := 2; i < 576; i++ {
		if xr[i] != 0 {
			t.Fatalf("xr[%d]=%g, want 0", i, xr[i])
		}
	}
}

func TestIntensityPanFormula(t *testing.T) {
	// kl = t/(1+t), kr = 1/(1+t), t = tan(pos × π/12)(ISO 11172-3 §2.4.3.4)
	for pos := 0; pos <= 6; pos++ {
		kl, kr := intensityPan(pos)
		tt := math.Tan(float64(pos) * math.Pi / 12)
		if math.Abs(kl-tt/(1+tt)) > 1e-12 || math.Abs(kr-1/(1+tt)) > 1e-12 {
			t.Fatalf("pos %d: (%g,%g)", pos, kl, kr)
		}
	}
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
