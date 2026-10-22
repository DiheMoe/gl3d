package gl3d

import "math"

// intensityPan 返回 MPEG-1 intensity stereo 的增益对 (kl, kr):
// t = tan(pos·π/12),kl = t/(1+t),kr = 1/(1+t)。
func intensityPan(pos int) (float64, float64) {
	t := math.Tan(float64(pos) * math.Pi / 12)
	return t / (1 + t), 1 / (1 + t)
}

// msStereo 就地对一对声道做 M/S 变换:l'=(l+r)/√2,r'=(l−r)/√2。
func msStereo(l, r []float64) {
	inv := 1 / math.Sqrt2
	for i := range l {
		m, s := l[i], r[i]
		l[i] = (m + s) * inv
		r[i] = (m - s) * inv
	}
}

// processStereo 处理一个 granule 的双声道谱线(反量化之后、IMDCT 之前)。
// intensity stereo 时右声道的 scalefactor 携带位置:MPEG-1 为 0..6(7 起为
// 该带不做 intensity 的哨兵);LSF 为幂次表,scalefacCompress&1 选表,-1 为
// 不做哨兵(LSF 终止值语义取自参考实现观察,各实现不一致,未经标准文本核实)。
// 最高带不传 scf,沿用下面一带(短块为同一窗口)的位置;下面一带不在
// intensity 区时各参考实现行为分歧,本实现仍取其 scalefactor。MS 只作用于
// intensity 界限以下的带与非法位置带;intensity 带不做 MS,也就不乘 MS 的
// 1/√2——本实现的 1/√2 只在 msStereo 中施加,不做 MS 的带无需补偿。
func processStereo(h *frameHeader, giR *granuleInfo, bm *bandMap, scfRight []int, transmitted int, l, r *[576]float64) {
	if h.channelMode != modeJointStereo {
		return
	}
	ms := h.modeExt&2 != 0
	if h.modeExt&1 == 0 {
		if ms {
			msStereo(l[:], r[:])
		}
		return
	}

	// intensity 界限:右声道最后一个非零带。短块带逐窗口确定;长块带(含混合块
	// 的长部)只在三个窗口的短块带全为零时才可能进入 intensity 区。
	maxShort := [3]int{-1, -1, -1}
	maxLong := -1
	for b := 0; b < bm.scfLen; b++ {
		lo, hi := bm.sfbStart[b], bm.sfbStart[b+1]
		for i := lo; i < hi; i++ {
			if r[i] != 0 {
				if w := bm.bandWin[b]; w >= 0 {
					maxShort[w] = b
				} else {
					maxLong = b
				}
				break
			}
		}
	}
	if maxShort != [3]int{-1, -1, -1} {
		maxLong = bm.scfLen
	}
	belowBound := func(b int) bool {
		if w := bm.bandWin[b]; w >= 0 {
			return b <= maxShort[w]
		}
		return b <= maxLong
	}

	maxPos := 64
	if h.version == MPEG1 {
		maxPos = 7
	}
	for b := 0; b < bm.scfLen; b++ {
		lo, hi := bm.sfbStart[b], bm.sfbStart[b+1]
		if belowBound(b) {
			if ms {
				msStereo(l[lo:hi], r[lo:hi])
			}
			continue
		}
		// 最高带不传 scalefactor,沿用下面一带(短块为同一窗口)的位置,
		// 非法位置也沿用
		src := b
		if b >= transmitted {
			src = b - 1
			if bm.bandWin[b] >= 0 {
				src = b - 3
			}
		}
		ipos := scfRight[src]
		if ipos < 0 || ipos >= maxPos {
			// 非法位置(LSF 为全 1 哨兵 -1):该带不做 intensity
			if ms {
				msStereo(l[lo:hi], r[lo:hi])
			}
			continue
		}
		var kl, kr float64
		if h.version == MPEG1 {
			kl, kr = intensityPan(ipos)
		} else {
			kl, kr = 1, math.Pow(2, -float64(((ipos+1)>>1)<<(giR.scalefacCompress&1))/4)
			if ipos&1 != 0 {
				kl, kr = kr, 1
			}
		}
		for i := lo; i < hi; i++ {
			r[i] = l[i] * kr
			l[i] = l[i] * kl
		}
	}
}
