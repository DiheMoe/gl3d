package gl3d

import (
	"math"
	"math/rand"
	"testing"
)

func TestIMDCTWindows(t *testing.T) {
	// 正常块:sin(π/36 (i+½))
	if got := math.Abs(mdctWindows[0][0] - math.Sin(math.Pi/72)); got > 1e-12 {
		t.Fatalf("w0[0]=%g", mdctWindows[0][0])
	}
	if math.Abs(mdctWindows[0][17]-0.99904822) > 1e-6 {
		t.Fatalf("w0[17]=%g, want ~0.99904822", mdctWindows[0][17])
	}
	// 起始块:18..23 为 1,30..35 为 0,24..29 为 12 点正弦尾
	for i := 18; i <= 23; i++ {
		if mdctWindows[1][i] != 1 {
			t.Fatalf("w1[%d]=%g, want 1", i, mdctWindows[1][i])
		}
	}
	for i := 30; i <= 35; i++ {
		if mdctWindows[1][i] != 0 {
			t.Fatalf("w1[%d]=%g, want 0", i, mdctWindows[1][i])
		}
	}
	if got := math.Abs(mdctWindows[1][24] - math.Sin(math.Pi/12*6.5)); got > 1e-12 {
		t.Fatalf("w1[24]=%g", mdctWindows[1][24])
	}
	// 结束块是起始块的时间镜像
	for i := 0; i < 36; i++ {
		if mdctWindows[3][i] != mdctWindows[1][35-i] {
			t.Fatalf("w3[%d]=%g != w1[%d]=%g", i, mdctWindows[3][i], 35-i, mdctWindows[1][35-i])
		}
	}
	if math.Abs(shortWindow[0]-math.Sin(math.Pi/24)) > 1e-12 || math.Abs(shortWindow[11]-math.Sin(math.Pi/24*23)) > 1e-12 {
		t.Fatalf("shortWindow endpoints: %g %g", shortWindow[0], shortWindow[11])
	}
}

func TestIMDCT36Delta(t *testing.T) {
	// 冲激输入 x[0]=1 → out[i] = cos(π/72 (2i+19))
	var x [18]float64
	x[0] = 1
	var out [36]float64
	imdct36(&x, &out)
	for i := 0; i < 36; i++ {
		want := math.Cos(math.Pi / 72 * float64(2*i+19))
		if math.Abs(out[i]-want) > 1e-12 {
			t.Fatalf("out[%d]=%g, want %g", i, out[i], want)
		}
	}
}

// 随机输入下逐点符合 ISO 11172-3 §2.4.3.4.6 的 IMDCT 定义:
// x_i = Σ_k X_k·cos(π/(2n)·(2i+1+n/2)(2k+1)),n 为输出点数。
func TestIMDCTMatchesDefinition(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	check := func(n int, in, out []float64) {
		for i := 0; i < n; i++ {
			want := 0.0
			for k := range in {
				want += in[k] * math.Cos(math.Pi/float64(2*n)*float64((2*i+1+n/2)*(2*k+1)))
			}
			if d := math.Abs(out[i] - want); d > 1e-12 {
				t.Fatalf("n=%d out[%d]=%.17g, want %.17g", n, i, out[i], want)
			}
		}
	}
	for trial := 0; trial < 10; trial++ {
		var x36 [18]float64
		for k := range x36 {
			x36[k] = rng.Float64()*2 - 1
		}
		var y36 [36]float64
		imdct36(&x36, &y36)
		check(36, x36[:], y36[:])

		var x12 [6]float64
		for k := range x12 {
			x12[k] = rng.Float64()*2 - 1
		}
		var y12 [12]float64
		imdct12(&x12, &y12)
		check(12, x12[:], y12[:])
	}
}

func TestIMDCT12Delta(t *testing.T) {
	var x [6]float64
	x[2] = 1
	var out [12]float64
	imdct12(&x, &out)
	for i := 0; i < 12; i++ {
		want := math.Cos(math.Pi / 24 * float64((2*i+7)*5))
		if math.Abs(out[i]-want) > 1e-12 {
			t.Fatalf("out[%d]=%g, want %g", i, out[i], want)
		}
	}
}

func TestOverlapAddLongBlocks(t *testing.T) {
	// 两个连续长块:第一块尾部进入 overlap,第二块输出含重叠和
	var st channelState
	var spec [576]float64
	for i := range spec {
		spec[i] = 0.01
	}
	var out1, out2 [32][18]float64
	s1, s2 := spec, spec // imdctBlock 就地抗混叠,每次传入同一输入的副本
	st.imdctBlock(&s1, 0, 0, nil, &out1)
	st.imdctBlock(&s2, 0, 0, nil, &out2)
	// 同一输入的第二块输出应不同于第一块(重叠相加生效),且都不为零
	nonzero1, nonzero2 := 0.0, 0.0
	for b := 0; b < 32; b++ {
		for j := 0; j < 18; j++ {
			nonzero1 += math.Abs(out1[b][j])
			nonzero2 += math.Abs(out2[b][j])
		}
	}
	if nonzero1 == 0 || nonzero2 == 0 {
		t.Fatalf("outputs zero: %g %g", nonzero1, nonzero2)
	}
	if nonzero1 == nonzero2 {
		t.Fatal("overlap-add has no effect")
	}
}

func TestShortBlockLayout(t *testing.T) {
	// 短块:窗口 0 的冲激应在 6..17 出现正弦形输出,0..5 与 30..35 为 0
	var st channelState
	var spec [576]float64
	// MPEG-1 44100 短块 sfb 带 0 宽度 4/窗:子带 0 覆盖窗口频率 0..5
	// 在带交错布局中,带 0 窗口 0 的 4 个值位于 spec[0..3]
	spec[0] = 1
	spec[1] = 0
	spec[2] = 0
	spec[3] = 0
	var out [32][18]float64
	st.imdctBlock(&spec, 2, 0, bandMapFor(MPEG1, 0, shapeShort), &out)
	// 子带 0、窗口 0:imdct12(x=[1,0,0,0,0,0])[i] = cos(π/24(2i+7)),加短窗,
	// 落在帧偏移 6..17
	for i := 0; i < 6; i++ {
		if out[0][i] != 0 {
			t.Fatalf("frame[%d]=%g, want 0 before first window", i, out[0][i])
		}
	}
	for i := 0; i < 12; i++ {
		want := math.Cos(math.Pi/24*float64(2*i+7)) * shortWindow[i]
		if math.Abs(out[0][6+i]-want) > 1e-12 {
			t.Fatalf("frame[%d]=%g, want %g", 6+i, out[0][6+i], want)
		}
	}
}

func TestAntialiasButterfly(t *testing.T) {
	// 构造两子带边界值,验证蝶形公式 u·cs − d·ca / u·ca + d·cs
	var spec [576]float64
	spec[17] = 0.5  // 子带 0 顶部
	spec[18] = -0.3 // 子带 1 底部
	antialias(&spec, 1)
	c, s := aliasCA[0], aliasCS[0]
	u, d := -0.3, 0.5
	// 下端(lower)= u·cs + d·ca;上端(upper)= d·cs − u·ca(dist10 III_antialias)
	if math.Abs(spec[18]-(u*s+d*c)) > 1e-12 {
		t.Fatalf("spec[18]=%g, want %g", spec[18], u*s+d*c)
	}
	if math.Abs(spec[17]-(d*s-u*c)) > 1e-12 {
		t.Fatalf("spec[17]=%g, want %g", spec[17], d*s-u*c)
	}
}
