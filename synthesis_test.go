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

// 合成窗关于 256 对称:n 不是 64 的倍数时
// D[512−n] = −D[n],否则相等。
func TestSynthesisWindowSymmetry(t *testing.T) {
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

// 直接交错写入必须保留另一声道,且在连续 granule 间保持滤波器历史。
func TestSynthesisGranuleInterleavedMatchesDefinition(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	var st synthState
	var ref refSynth
	for gr := 0; gr < 4; gr++ {
		var td [32][18]float64
		for sb := range td {
			for j := range td[sb] {
				td[sb][j] = rng.Float64()*2 - 1
			}
		}
		var pcm [1152]float64
		for i := range pcm {
			pcm[i] = 123
		}
		synthGranule(&st, &td, pcm[:], 2)
		for j := 0; j < 18; j++ {
			var col [32]float64
			for sb := range col {
				col[sb] = td[sb][j]
			}
			want := ref.column(&col)
			for i, v := range want {
				p := 2 * (j*32 + i)
				if diff := math.Abs(pcm[p] - v); diff > 1e-12 {
					t.Fatalf("granule %d sample %d: got %.17g, want %.17g", gr, p/2, pcm[p], v)
				}
				if pcm[p+1] != 123 {
					t.Fatalf("overwrote other channel at %d", p+1)
				}
			}
		}
	}
}
