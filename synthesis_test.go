package gl3d

import (
	"math"
	"math/rand"
	"testing"
)

// refSynth 按 ISO 11172-3 §2.4.3.2.5 流程图逐式计算合成滤波器组,作为测试
// 参照:V 移位 64,V[i] = Σ N[i][k]·S[k],U 由 V 拼装,W = U·D,输出为 W 的
// 16 项和。
type refSynth struct{ v [1024]float64 }

func (r *refSynth) column(s *[32]float64) [32]float64 {
	copy(r.v[64:], r.v[:960])
	for i := 0; i < 64; i++ {
		sum := 0.0
		for k := 0; k < 32; k++ {
			sum += math.Cos(float64((16+i)*(2*k+1))*math.Pi/64) * s[k]
		}
		r.v[i] = sum
	}
	var u [512]float64
	for i := 0; i < 8; i++ {
		for j := 0; j < 32; j++ {
			u[i*64+j] = r.v[i*128+j]
			u[i*64+32+j] = r.v[i*128+96+j]
		}
	}
	var out [32]float64
	for j := 0; j < 32; j++ {
		for i := 0; i < 16; i++ {
			out[j] += u[j+32*i] * synthWindowD[j+32*i]
		}
	}
	return out
}

func TestSynthesisMatchesISODefinition(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var st synthState
	var ref refSynth
	for c := 0; c < 64; c++ {
		var col, out [32]float64
		for k := range col {
			col[k] = rng.Float64()*2 - 1
		}
		st.synthColumn(&col, &out)
		want := ref.column(&col)
		for i := range out {
			if d := math.Abs(out[i] - want[i]); d > 1e-12 {
				t.Fatalf("column %d sample %d: got %.17g, want %.17g (diff %g)", c, i, out[i], want[i], d)
			}
		}
	}
}

// TestSynthesisWindowExactQ16 检查合成窗的两条结构性质:每个系数都是 2^-16
// 的整数倍(ISO 表的 9 位小数是这些值的舍入,minimp3 与 FFmpeg 以整数存储
// 同一组值);关于 256 对称,n 不是 64 的倍数时 D[512−n] = −D[n],否则相等。
func TestSynthesisWindowExactQ16(t *testing.T) {
	for n, d := range synthWindowD {
		if k := d * 65536; k != math.Trunc(k) {
			t.Fatalf("D[%d] = %.17g is not a multiple of 2^-16", n, d)
		}
	}
	for n := 1; n < 256; n++ {
		want := -synthWindowD[n]
		if n%64 == 0 {
			want = synthWindowD[n]
		}
		if got := synthWindowD[512-n]; got != want {
			t.Errorf("D[%d] = %g, want %g (mirror of D[%d])", 512-n, got, want, n)
		}
	}
}

func TestDCTIIMatchesDefinition(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	dct := map[int]func([]float64){
		4:  func(x []float64) { dct4((*[4]float64)(x)) },
		8:  func(x []float64) { dct8((*[8]float64)(x)) },
		16: func(x []float64) { dct16((*[16]float64)(x)) },
		32: func(x []float64) { dct32((*[32]float64)(x)) },
	}
	for _, n := range []int{4, 8, 16, 32} {
		x := make([]float64, n)
		for i := range x {
			x[i] = rng.Float64()*2 - 1
		}
		want := make([]float64, n)
		for j := range want {
			for k := range x {
				want[j] += x[k] * math.Cos(float64(j*(2*k+1))*math.Pi/float64(2*n))
			}
		}
		got := append([]float64(nil), x...)
		dct[n](got)
		for j := range got {
			if d := math.Abs(got[j] - want[j]); d > 1e-12 {
				t.Fatalf("n=%d X[%d]: got %.17g, want %.17g", n, j, got[j], want[j])
			}
		}
	}
}

func TestSynthesisZeroInput(t *testing.T) {
	var st synthState
	var col [32]float64
	var out [32]float64
	for c := 0; c < 20; c++ {
		st.synthColumn(&col, &out)
	}
	for i := range out {
		if out[i] != 0 {
			t.Fatalf("out[%d]=%g, want 0", i, out[i])
		}
	}
}

func TestSynthesisDCConverges(t *testing.T) {
	// 子带 0 持续输入直流:稳态输出应收敛到有限非零值(滤波器组是低通的)
	var st synthState
	var col [32]float64
	var out [32]float64
	var prev float64
	converged := false
	for c := 0; c < 40; c++ {
		col[0] = 1
		st.synthColumn(&col, &out)
		if c > 0 && math.Abs(out[0]-prev) < 1e-9 && out[0] != 0 {
			converged = true
		}
		prev = out[0]
	}
	if !converged {
		t.Fatal("DC input did not converge to steady output")
	}
}

func TestSynthesisStateEvolution(t *testing.T) {
	// 冲激输入:输出是滤波器脉冲响应的滚动展开,两个 granule 间状态连续
	var st synthState
	var col [32]float64
	var out [32]float64
	col[0] = 1
	st.synthColumn(&col, &out)
	first := out
	col[0] = 0
	st.synthColumn(&col, &out)
	second := out
	same := true
	for i := range first {
		if first[i] != second[i] {
			same = false
		}
	}
	if same {
		t.Fatal("filter state did not evolve between columns")
	}
}
