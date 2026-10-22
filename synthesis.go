package gl3d

import "math"

// 多相合成滤波器组:V 为 1024 样本环形缓冲,矩阵乘 V[i] = Σ N[i][k]·S[k]
// (N[i][k] = cos((16+i)(2k+1)π/64))经 32 点 DCT-II 与对称关系计算,U 按
// 128j+i / 128j+96+i 拼装,W = U×D(D 为 synthesis_window.go 中的协议常数
// 合成窗),32 个 PCM 各由 16 个间隔 32 的 W 求和。

// Lee 分解各级系数 1/(2·cos((2i+1)π/(2n))),i < n/2;init 按公式生成。
var (
	dctC32 [16]float64
	dctC16 [8]float64
	dctC8  [4]float64
	dctC4  [2]float64
	dctC2  float64
)

func init() {
	coef := func(c []float64) {
		n := 2 * len(c)
		for i := range c {
			c[i] = 1 / (2 * math.Cos(float64(2*i+1)*math.Pi/float64(2*n)))
		}
	}
	coef(dctC32[:])
	coef(dctC16[:])
	coef(dctC8[:])
	coef(dctC4[:])
	dctC2 = 1 / (2 * math.Cos(math.Pi/4))
}

// dct32 就地计算不归一化的 32 点 DCT-II:X[j] = Σ_k x[k]·cos(j(2k+1)π/64)。
// 按 B. G. Lee(1984)的递归分解逐级展开:偶数项是对称和序列的半长 DCT-II,
// 奇数项是加权差序列半长 DCT-II 的相邻两项之和。
func dct32(x *[32]float64) {
	var a, b [16]float64
	for i := 0; i < 16; i++ {
		u, v := x[i], x[31-i]
		a[i] = u + v
		b[i] = (u - v) * dctC32[i]
	}
	dct16(&a)
	dct16(&b)
	for k := 0; k < 15; k++ {
		x[2*k] = a[k]
		x[2*k+1] = b[k] + b[k+1]
	}
	x[30] = a[15]
	x[31] = b[15]
}

func dct16(x *[16]float64) {
	var a, b [8]float64
	for i := 0; i < 8; i++ {
		u, v := x[i], x[15-i]
		a[i] = u + v
		b[i] = (u - v) * dctC16[i]
	}
	dct8(&a)
	dct8(&b)
	for k := 0; k < 7; k++ {
		x[2*k] = a[k]
		x[2*k+1] = b[k] + b[k+1]
	}
	x[14] = a[7]
	x[15] = b[7]
}

func dct8(x *[8]float64) {
	var a, b [4]float64
	for i := 0; i < 4; i++ {
		u, v := x[i], x[7-i]
		a[i] = u + v
		b[i] = (u - v) * dctC8[i]
	}
	dct4(&a)
	dct4(&b)
	for k := 0; k < 3; k++ {
		x[2*k] = a[k]
		x[2*k+1] = b[k] + b[k+1]
	}
	x[6] = a[3]
	x[7] = b[3]
}

// dct4 含展开的 2 点级:X0 = a0+a1,X1 = (a0−a1)·c2。
func dct4(x *[4]float64) {
	a0, a1 := x[0]+x[3], x[1]+x[2]
	b0, b1 := (x[0]-x[3])*dctC4[0], (x[1]-x[2])*dctC4[1]
	bb1 := (b0 - b1) * dctC2
	x[0] = a0 + a1
	x[1] = (b0 + b1) + bb1
	x[2] = (a0 - a1) * dctC2
	x[3] = bb1
}

// synthState 是单声道的合成滤波器组状态。v 为 1024 样本环形缓冲的双份镜像
// (v[i] 与 v[i+1024] 恒等),使加窗读取无需回绕。
type synthState struct {
	v   [2048]float64
	pos int
}

// synthColumn 处理一列 32 个子带采样,输出 32 个 PCM 样本。
// 记 X 为 S 的 32 点 DCT-II,由余弦的周期与对称性,V[i] = Σ_k N[i][k]·S[k]
// 在 i<16 时为 X[16+i],i=16 时为 0,17≤i<48 时为 −X[48−i],i≥48 时为 −X[i−48]。
func (st *synthState) synthColumn(s *[32]float64, out *[32]float64) {
	x := *s
	dct32(&x)
	st.pos = (st.pos + 1024 - 64) & 1023
	v := (*[64]float64)(st.v[st.pos : st.pos+64])
	for i := 0; i < 16; i++ {
		v[i] = x[16+i]
		v[48+i] = -x[i]
	}
	v[16] = 0
	for i := 17; i < 48; i++ {
		v[i] = -x[48-i]
	}
	copy(st.v[st.pos+1024:st.pos+1088], v[:])
	// U 拼装与加窗合并:第 j 段 U[32j+i] = V[pos + 32j + 64·⌊(j+1)/2⌋ + i],
	// 段偏移依次为 0,96,128,224,…,992
	w := (*[1024]float64)(st.v[st.pos : st.pos+1024])
	d := &synthWindowD
	for i := 0; i < 32; i++ {
		out[i] = w[i]*d[i] + w[96+i]*d[32+i] + w[128+i]*d[64+i] + w[224+i]*d[96+i] +
			w[256+i]*d[128+i] + w[352+i]*d[160+i] + w[384+i]*d[192+i] + w[480+i]*d[224+i] +
			w[512+i]*d[256+i] + w[608+i]*d[288+i] + w[640+i]*d[320+i] + w[736+i]*d[352+i] +
			w[768+i]*d[384+i] + w[864+i]*d[416+i] + w[896+i]*d[448+i] + w[992+i]*d[480+i]
	}
}

// synthGranule 把 IMDCT 输出的子带时域(子带主序 [32][18])合成为 576 个
// PCM 样本,第 n 个写入 pcm[n*stride](交错输出时 stride 为声道数)。
func synthGranule(st *synthState, td *[32][18]float64, pcm []float64, stride int) {
	pcm = pcm[:575*stride+1]
	for j := 0; j < 18; j++ {
		var col, out [32]float64
		for k := 0; k < 32; k++ {
			col[k] = td[k][j]
		}
		st.synthColumn(&col, &out)
		p := pcm[j*32*stride:]
		for i, v := range out {
			p[i*stride] = v
		}
	}
}
