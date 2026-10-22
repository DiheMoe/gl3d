package gl3d

import "math"

// IMDCT、加窗、重叠相加与抗混叠蝶形,按 ISO 11172-3 的公式直接实现。
// 余弦表与窗在 init 时由定义公式生成,源码不含大字面量表。

var (
	cosIMDCT36  [36][18]float64 // cos(π/72 (2i+19)(2k+1))
	cosIMDCT12  [12][6]float64  // cos(π/24 (2i+7)(2k+1))
	mdctWindows [4][36]float64  // 0=正常 1=起始 2=短(占位不用) 3=结束
	shortWindow [12]float64     // sin(π/12 (i+½))
	aliasCS     [8]float64
	aliasCA     [8]float64
)

func init() {
	for i := 0; i < 36; i++ {
		for k := 0; k < 18; k++ {
			cosIMDCT36[i][k] = math.Cos(math.Pi / 72 * float64((2*i+19)*(2*k+1)))
		}
	}
	for i := 0; i < 12; i++ {
		for k := 0; k < 6; k++ {
			cosIMDCT12[i][k] = math.Cos(math.Pi / 24 * float64((2*i+7)*(2*k+1)))
		}
		shortWindow[i] = math.Sin(math.Pi / 12 * (float64(i) + 0.5))
	}
	for i := 0; i < 36; i++ {
		n := math.Sin(math.Pi / 36 * (float64(i) + 0.5))
		mdctWindows[0][i] = n
		switch {
		case i < 18:
			mdctWindows[1][i] = n
		case i < 24:
			mdctWindows[1][i] = 1
		case i < 30:
			mdctWindows[1][i] = math.Sin(math.Pi / 12 * (float64(i-18) + 0.5))
		}
	}
	// 结束块窗是起始块窗的时间镜像,须在起始块窗填完后进行
	for i := 0; i < 36; i++ {
		mdctWindows[3][i] = mdctWindows[1][35-i]
	}
	// ci 取自 ISO 11172-3 Table B.9(负值);cs = 1/√(1+ci²),ca = ci·cs
	ci := [8]float64{-0.6, -0.535, -0.33, -0.185, -0.095, -0.041, -0.0142, -0.0037}
	for i, c := range ci {
		cs := 1 / math.Sqrt(1+c*c)
		aliasCS[i] = cs
		aliasCA[i] = c * cs
	}
}

// imdct36 是 18 输入 → 36 输出的 IMDCT。输出前半关于 8.5 反对称、后半关于
// 26.5 对称(y[17−i] = −y[i],y[53−i] = y[i]),只需计算 y[0..8] 与 y[18..26]。
func imdct36(x *[18]float64, out *[36]float64) {
	for i := 0; i < 9; i++ {
		c1, c2 := &cosIMDCT36[i], &cosIMDCT36[18+i]
		s1, s2 := 0.0, 0.0
		for k := 0; k < 18; k++ {
			s1 += x[k] * c1[k]
			s2 += x[k] * c2[k]
		}
		out[i], out[17-i] = s1, -s1
		out[18+i], out[35-i] = s2, s2
	}
}

// imdct12 是 6 输入 → 12 输出的 IMDCT(短块每窗口),同理 y[5−i] = −y[i]、
// y[17−i] = y[i],只需计算 y[0..2] 与 y[6..8]。
func imdct12(x *[6]float64, out *[12]float64) {
	for i := 0; i < 3; i++ {
		c1, c2 := &cosIMDCT12[i], &cosIMDCT12[6+i]
		s1, s2 := 0.0, 0.0
		for k := 0; k < 6; k++ {
			s1 += x[k] * c1[k]
			s2 += x[k] * c2[k]
		}
		out[i], out[5-i] = s1, -s1
		out[6+i], out[11-i] = s2, s2
	}
}

// antialias 对前 aaBands+1 个子带做蝶形抗混叠:相邻子带在当前子带上端 8 样本
// 与下一子带下端 8 样本之间做蝶形(bu' = bu·cs − bd·ca,bd' = bd·cs + bu·ca)。
// 含短块的 granule 只在长块子带部分进行(纯短块不做)。
func antialias(spec *[576]float64, aaBands int) {
	for b := 0; b < aaBands; b++ {
		base := b * 18
		for i := 0; i < 8; i++ {
			u := spec[base+18+i] // 下一子带下端(bd)
			d := spec[base+17-i] // 当前子带上端(bu)
			spec[base+18+i] = u*aliasCS[i] + d*aliasCA[i]
			spec[base+17-i] = d*aliasCS[i] - u*aliasCA[i]
		}
	}
}

// channelState 是单声道的 IMDCT 重叠状态与短块重排缓冲。
type channelState struct {
	overlap [32][18]float64
	reorder [576]float64
}

// reorderShortPart 把短块区域的谱线从带交错布局([带][窗][频])重排为频主序
// 三元组([频][窗]):des = sfb_start*3 + freq*3 + window,imdct12 以
// in[w + 3m] 读取。
func reorderShortPart(dst, src *[576]float64, bm *bandMap, nLongSubbands int) {
	copy(dst[:nLongSubbands*18], src[:nLongSubbands*18])
	base := nLongSubbands * 18
	for _, b := range bm.shortBands {
		for f := 0; f < b.width; f++ {
			for w := 0; w < 3; w++ {
				dst[base+b.freqBase*3+f*3+w] = src[b.posA+w*b.width+f]
			}
		}
	}
}

// imdctBlock 把一个声道的 576 条谱线变为 576 个子带时域采样,写入 out
// (子带主序 [32][18]);spec 被就地抗混叠,调用后不再保留原值。blockType:
// 0 长、1 起始、2 短、3 结束。nLongSubbands 是混合块(mixed_block_flag 置位)
// 中用正常窗变换的最低子带数,0 表示不是混合块:短块时这些子带走长块路径,
// 起始与结束块时只换成正常窗。MPEG-1/2 为 2;MPEG-2.5 @ 8 kHz 为 4(参考实现
// 观察值,未经标准文本核实)。
func (st *channelState) imdctBlock(spec *[576]float64, blockType int, nLongSubbands int, bm *bandMap, out *[32][18]float64) {
	work := spec
	aa := 31
	if blockType == 2 {
		clear(st.reorder[:])
		reorderShortPart(&st.reorder, spec, bm, nLongSubbands)
		work = &st.reorder
		aa = nLongSubbands - 1
	}
	if aa > 0 {
		antialias(work, aa)
	}
	wt := blockType
	if blockType == 2 {
		wt = 0 // 混合块长部用正常窗
	}
	win := &mdctWindows[wt]
	for sb := 0; sb < 32; sb++ {
		in := (*[18]float64)(work[sb*18 : sb*18+18])
		ov, o := &st.overlap[sb], &out[sb]
		switch {
		case *in == [18]float64{}:
			// 零输入的 IMDCT 为零:本块输出即上一块的重叠部分
			*o = *ov
			*ov = [18]float64{}
		case blockType == 2 && sb >= nLongSubbands:
			var frame [36]float64
			for w := 0; w < 3; w++ {
				var x [6]float64
				for m := 0; m < 6; m++ {
					x[m] = in[w+3*m] // 频主序三元组布局
				}
				var y [12]float64
				imdct12(&x, &y)
				for i := 0; i < 12; i++ {
					frame[6+6*w+i] += y[i] * shortWindow[i]
				}
			}
			for j := 0; j < 18; j++ {
				o[j] = frame[j] + ov[j]
				ov[j] = frame[18+j]
			}
		default:
			w := win
			if sb < nLongSubbands {
				w = &mdctWindows[0] // 混合块的长部用正常窗,起始与结束块也如此
			}
			var y [36]float64
			imdct36(in, &y)
			for j := 0; j < 18; j++ {
				o[j] = y[j]*w[j] + ov[j]
				ov[j] = y[18+j] * w[18+j]
			}
		}
		// 频谱反相(多相滤波器组接口约定):奇数子带的奇数时隙取负
		if sb%2 == 1 {
			for j := 1; j < 18; j += 2 {
				o[j] = -o[j]
			}
		}
	}
}
