package gl3d

import (
	"math"
)

// scalefactor 读取与反量化。带宽、分区与混合基数表是 tabgen 生成的协议事实
// (sfb_tables.go)。本文件除 scfSlen 外只含算法,不含表数据。

type blockShape int

const (
	shapeLong blockShape = iota
	shapeShort
	shapeMixed
)

// scfSlen 是 MPEG-1 scalefac_compress 到两段 scalefactor 位宽 (slen1, slen2)
// 的映射表,取值与 mpg123 的 slen 表、minimp3 的 g_scfc_decode 表逐项一致。
var scfSlen = [2][16]int{
	{0, 0, 0, 0, 3, 1, 1, 1, 2, 2, 2, 3, 3, 3, 4, 4},
	{0, 1, 2, 3, 0, 1, 2, 3, 1, 2, 3, 1, 2, 3, 2, 3},
}

// pretab 是 preflag 对长块 scalefactor 带的附加增益表。
var pretab [22]int

func init() {
	for i, v := range pretabTail[0] {
		pretab[11+i] = int(v)
	}
}

// srSfbRow 把版本与版本内采样率序号映射到 sfb 带宽表的行。
// MPEG-2.5 的 11025 Hz 与 12000 Hz 共用一行。
func srSfbRow(v mpegVersion, srIdx int) int {
	base := 0
	switch v {
	case mpeg1:
		base = 6
	case mpeg2:
		base = 3
	}
	r := base + srIdx
	if r > 0 {
		r--
	}
	return r
}

// bandMap 是 granule 的谱线到 scalefactor 带/窗口/区域的映射。
type bandMap struct {
	scfIdx   [576]int // 谱线 → scalefactor 下标
	window   [576]uint8
	short    [576]bool // 谱线属短块部分(subblock_gain 适用)
	sfbStart []int     // 每个 scf 带的起始谱线(用于 region 边界)
	scfLen   int       // 有效 scf 数(含末尾零哨兵)
	long     bool      // 纯长块(preflag 的 pretab 适用)
	bandWin  [40]int8  // 每个 scf 带的短块窗口号,长块带为 -1(subblock_gain 选择)

	// shortBands 描述短块区各 sfb 带的重排参数(带交错布局 → 子带主序)。
	shortBands []shortBandInfo
}

// shortBandInfo 是短块区一个 sfb 带在带交错布局中的位置与宽度。
type shortBandInfo struct {
	posA     int // 带在谱线数组中的起始位置(窗口 0)
	width    int // 每窗口宽度
	freqBase int // 短区频率轴上的起点(每窗口)
}

var bandMaps [3][3][3]*bandMap

func bandMapFor(v mpegVersion, srIdx int, shape blockShape) *bandMap {
	return bandMaps[v][srIdx][shape]
}

func init() {
	for v := 0; v < 3; v++ {
		nLong := 8
		if mpegVersion(v) != mpeg1 {
			nLong = 6
		}
		for r := 0; r < 3; r++ {
			row := srSfbRow(mpegVersion(v), r)
			bandMaps[v][r][shapeLong] = buildBandMap(sfbLongWidths[row][:], 22, 0, true)
			bandMaps[v][r][shapeShort] = buildBandMap(sfbShortWidths[row][:], 0, 39, false)
			bandMaps[v][r][shapeMixed] = buildBandMap(mixedWidths(row, nLong), nLong, 30, false)
		}
	}
}

// mixedWidths 返回混合块的带宽:长块表的前 nLong 带,接短块表第 3 带起的 30 个
// (带,窗)宽度。两张表的这两段在同一条谱线相接(8 kHz 为 72,其余为 36)。
func mixedWidths(row, nLong int) []uint8 {
	w := append([]uint8(nil), sfbLongWidths[row][:nLong]...)
	return append(w, sfbShortWidths[row][9:39]...)
}

// buildBandMap 从带宽表构建映射。nLong+nShort 为有效带数。短块部分按窗口交错。
func buildBandMap(widths []uint8, nLong, nShort int, long bool) *bandMap {
	bm := &bandMap{scfLen: nLong + nShort}
	pos := 0
	band := 0
	addBand := func(w int, win uint8, isShort bool) {
		bm.bandWin[band] = -1
		if isShort {
			bm.bandWin[band] = int8(win)
		}
		for k := 0; k < w && pos < 576; k++ {
			bm.scfIdx[pos] = band
			bm.window[pos] = win
			bm.short[pos] = isShort
			pos++
		}
		bm.sfbStart = append(bm.sfbStart, pos-w)
		band++
	}
	for i := 0; i < nLong; i++ {
		addBand(int(widths[i]), 0, false)
	}
	// 短块部分:带宽按 (带,窗口) 交错存储,每带连续 3 个窗口宽度。
	// 同时记录重排参数(imdct.go reorderShortPart 用)
	posA := 0
	for i := 0; i < nLong; i++ {
		posA += int(widths[i])
	}
	fb := 0
	for b := 0; b*3 < nShort; b++ {
		wb := int(widths[nLong+b*3])
		bm.shortBands = append(bm.shortBands, shortBandInfo{posA: posA, width: wb, freqBase: fb})
		posA += wb * 3
		fb += wb
		for w := 0; w < 3; w++ {
			addBand(int(widths[nLong+b*3+w]), uint8(w), true)
		}
	}
	for pos < 576 {
		bm.scfIdx[pos] = bm.scfLen - 1 // 尾部归入哨兵带
		pos++
	}
	bm.sfbStart = append(bm.sfbStart, 576)
	bm.long = long
	return bm
}

// lsfScfSizes 用混合基数分解把 LSF 9 位 scalefac_compress 展开为 4 段 scalefactor
// 位宽和分区变体。intensity 右声道的字段先右移 1 位,变体行号偏移 12。右移后为 255
// 时 ISO 13818-3 没有给出对应的行,这里按 244–254 的公式展开,与 FFmpeg、mpg123 相同。
// 变体 = 消费的行数 × 4,匹配成功的一行也计入(minimp3 的循环退出语义)。
func lsfScfSizes(sfc int, intensityR bool) ([4]int, int, error) {
	var sizes [4]int
	ist := 0
	if intensityR {
		ist = 1
	}
	sfc >>= ist
	k := ist * 12
	for sfc >= 0 {
		if k+4 > 24 {
			return sizes, 0, &CorruptError{Frame: -1, Detail: "invalid LSF scalefac_compress"}
		}
		modprod := 1
		for i := 3; i >= 0; i-- {
			m := int(scfMod[0][k+i])
			sizes[i] = sfc / modprod % m
			modprod *= m
		}
		sfc -= modprod
		k += 4
	}
	return sizes, k, nil
}

// scfShapeRow 返回块形在 scfPartitions 中的行号(分区表的固定行序:长、混合、短)。
func scfShapeRow(shape blockShape) int {
	switch shape {
	case shapeShort:
		return 2
	case shapeMixed:
		return 1
	default:
		return 0
	}
}

// readScalefactors 读取一个 granule/声道的 scalefactor 序列。每帧第二个
// granule 中 scfsi 置位的组复用第一个 granule 的值、不再占位(仅 MPEG-1 有
// scfsi)。短块与混合块的组 1、2 永不复用(minimp3 同款约束)。intensityR 为
// intensity stereo 的右声道。LSF 时 ist 中带内全 1 的哨兵改写为 -1,表示该带
// 不做 intensity。结果写入 scf(长度为该块形的 scf 数)与 ist(仅 intensityR 时
// 使用)。返回实际传输的带数,其后为哨兵带(值为 0)。
func readScalefactors(br *bitReader, v mpegVersion, gi *granuleInfo, scfsi [4]int, shape blockShape, granule2 bool, prevScf []int, intensityR bool, scf, ist []int) (int, error) {
	prow := scfShapeRow(shape)
	var sizes [4]int
	var counts [4]int
	var countsU8 []uint8
	if v.lsf() {
		s, variant, err := lsfScfSizes(gi.scalefacCompress, intensityR)
		if err != nil {
			return 0, err
		}
		sizes = s
		countsU8 = scfPartitions[prow][variant : variant+4]
	} else {
		s1, s2 := scfSlen[0][gi.scalefacCompress], scfSlen[1][gi.scalefacCompress]
		sizes = [4]int{s1, s1, s2, s2}
		countsU8 = scfPartitions[prow][0:4]
	}
	for i := range countsU8 {
		counts[i] = int(countsU8[i])
	}
	clear(scf)
	if intensityR {
		clear(ist)
	}
	pos := 0
	for g := 0; g < 4; g++ {
		cnt := int(counts[g])
		if cnt == 0 {
			continue
		}
		if granule2 && scfsi[g] == 1 && !(shape != shapeLong && (g == 1 || g == 2)) {
			copy(scf[pos:pos+cnt], prevScf[pos:pos+cnt])
			if intensityR {
				copy(ist[pos:pos+cnt], scf[pos:pos+cnt])
			}
			pos += cnt
			continue
		}
		for i := 0; i < cnt; i++ {
			if sizes[g] == 0 {
				pos++
				continue
			}
			val, err := br.read(sizes[g])
			if err != nil {
				return 0, &CorruptError{Frame: -1, Detail: "scalefactor data underrun"}
			}
			scf[pos] = val
			if intensityR {
				ist[pos] = val
				if v.lsf() && val == (1<<sizes[g])-1 {
					ist[pos] = -1 // LSF intensity:全 1 哨兵 = 该带不做 intensity
				}
			}
			pos++
		}
	}
	return pos, nil
}

// regionBounds 返回三个 region 的谱线边界。长块与起始/结束块的 regionCount 以长块
// sfb 带为单位。纯短块以交错的 (带,窗) 条目为单位,regionCount0=8 即 9 条目 =
// 3 带 × 3 窗,与 mpg123/dist10 的固定边界等价。块类型 2 的混合块 region0 止于
// 长块区上端,与 FFmpeg 相同。不这样截断的话,按条目计数的 region0 在 LSF 的
// 混合带上会越过长块区。
func regionBounds(gi *granuleInfo, bm *bandMap) [4]int {
	maxEntry := len(bm.sfbStart) - 1
	clamp := func(b int) int {
		if b > maxEntry {
			b = maxEntry
		}
		return b
	}
	r1 := bm.sfbStart[clamp(gi.regionCount[0]+1)]
	if gi.shape() == shapeMixed {
		r1 = bm.shortBands[0].posA
	}
	return [4]int{0, r1, bm.sfbStart[clamp(gi.regionCount[0]+gi.regionCount[1]+2)], 576}
}

// granuleSpectrum 是一个 granule/声道的解码结果:反量化谱线与立体声处理
// 所需的右声道 scalefactor 信息。由解码器持有并在 granule 间复用。
type granuleSpectrum struct {
	xr          [576]float64
	scf         [40]int
	ist         [40]int // 仅 intensity 右声道(hasIst)。LSF 带内全 1 哨兵为 -1
	hasIst      bool
	transmitted int // 实际传输的 scf 带数。0 表示 scalefactor 出错已置零,processStereo 据此不做 intensity
}

// decodeSpectra 解码并反量化一个 granule/channel 的 576 条谱线。位消耗以
// part2_3_length 为硬上限,超出即格式损坏。末尾不足一个完整 count1 四元组的位作为
// stuffing 丢弃。出错时 gs 仍是确定的频谱。part2_3_length 越过实际数据或 scalefactor
// 出错时整个 granule 置零,谱数据出错保留此前完整解出的谱线、余下置零。读位置总是
// 停在本 granule 的结尾,调用方容错后可以接着解码。
func decodeSpectra(h *frameHeader, si *sideInfo, gr, ch int, br *bitReader, bm *bandMap, shape blockShape, prevScf []int, intensityR bool, gs *granuleSpectrum) error {
	gi := &si.gr[gr][ch]
	limit := br.pos + gi.part2_3Length
	scf := gs.scf[:bm.scfLen]
	var transmitted int
	var err error
	// 表 0 不读位,只靠读位时的越界检查发现不了超出实际数据的声明
	if limit > len(br.data)*8 {
		err = &CorruptError{Frame: -1, Detail: "part2_3_length exceeds main data"}
	} else if transmitted, err = readScalefactors(br, h.version, gi, si.scfsi[ch], shape, gr == 1, prevScf, intensityR, scf, gs.ist[:bm.scfLen]); err == nil && br.pos > limit {
		err = &CorruptError{Frame: -1, Detail: "part2_3_length exceeded by scalefactors"}
	}
	if err != nil {
		clear(scf)
		clear(gs.ist[:bm.scfLen])
		gs.hasIst, gs.transmitted = intensityR, 0
		clear(gs.xr[:])
		br.pos = limit
		return err
	}
	gs.hasIst, gs.transmitted = intensityR, transmitted
	var is [576]int
	nz, err := decodeHuffman(br, gi, bm, limit, &is)
	br.pos = limit
	dequantize(gi, bm, scf, &is, nz, &gs.xr)
	return err
}

// decodeHuffman 解出 big_values 区与 count1 区的量化值写入 is,返回有值的谱线数。出错时
// 返回此前完整解出的谱线数与错误。
func decodeHuffman(br *bitReader, gi *granuleInfo, bm *bandMap, limit int, is *[576]int) (int, error) {
	pos := 0
	rb := regionBounds(gi, bm)
	ireg := 0
	for pairs := gi.bigValues; pairs > 0; pairs-- {
		for pos >= rb[ireg+1] && ireg < 2 {
			ireg++
		}
		x, y, err := huff.decodePair(br, gi.tableSelect[ireg])
		if err != nil {
			return pos, err
		}
		if br.pos > limit {
			return pos, &CorruptError{Frame: -1, Detail: "part2_3_length exceeded by huffman data"}
		}
		is[pos], is[pos+1] = x, y
		pos += 2
	}
	// count1 四元组,直到 granule 位耗尽。跨界四元组丢弃(参考实现一致行为)
	for pos+4 <= 576 && br.pos < limit {
		q, complete := decodeQuad(br, gi.count1Table == 1, limit)
		if !complete {
			break
		}
		copy(is[pos:pos+4], q[:])
		pos += 4
	}
	return pos, nil
}

// pow43[n] = n^(4/3),覆盖 big_values 的最大幅度 15 + 2^13 − 1 = 8206
// (linbits 至多 13 位)。init 按公式生成。
var pow43 [8207]float64

// 增益指数是 q/4。global_gain 为 0..255,scalefactor 至多 31(LSF 最宽
// 5 位),scalefac_scale 为 0..1,subblock_gain 为 0..7,pretab 至多 3。
// 短区 q >= -210-8*7-4*31 = -390。长区 q >= -210-4*(31+3) = -346。
// 两者 q <= 255-210 = 45。字段范围由帧头/side info/读位宽度保证。
const minGainQuarter, maxGainQuarter = -390, 45

var gainQuarter [maxGainQuarter - minGainQuarter + 1]float64

func init() {
	for i := range pow43 {
		pow43[i] = math.Pow(float64(i), 4.0/3.0)
	}
	for i := range gainQuarter {
		gainQuarter[i] = math.Exp2(float64(i+minGainQuarter) / 4)
	}
}

// dequantize 反量化并写入 xr 全部 576 条谱线:
// xr[i] = sign(is)·|is|^(4/3) · 2^((global_gain−210−8·subblock_gain[w])/4 − (scalefac_scale+1)·scf/2),
// 长块 preflag 另加 pretab。增益按 scalefactor 带计算一次。nz 之后的谱线为 0。
func dequantize(gi *granuleInfo, bm *bandMap, scf []int, is *[576]int, nz int, xr *[576]float64) {
	var gain [40]float64
	sfs := 2 * (gi.scalefacScale + 1)
	for b := 0; b < bm.scfLen; b++ {
		s := scf[b]
		if gi.preflag != 0 && bm.long && b < 22 {
			s += pretab[b]
		}
		q := gi.globalGain - 210 - sfs*s
		if w := bm.bandWin[b]; w >= 0 {
			q -= 8 * gi.subblockGain[w]
		}
		gain[b] = gainQuarter[q-minGainQuarter]
	}
	for i := 0; i < nz; i++ {
		switch v := is[i]; {
		case v > 0:
			xr[i] = pow43[v] * gain[bm.scfIdx[i]]
		case v < 0:
			xr[i] = -pow43[-v] * gain[bm.scfIdx[i]]
		default:
			xr[i] = 0
		}
	}
	clear(xr[nz:])
}
