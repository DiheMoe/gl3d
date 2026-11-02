package gl3d

import "fmt"

// side information 解析。LSF(MPEG-2/2.5)相对 MPEG-1 的差异:main_data_begin
// 8 bit、scalefac_compress 9 bit、无 scfsi、单 granule、preflag 派生。

type granuleInfo struct {
	part2_3Length    int
	bigValues        int
	globalGain       int
	scalefacCompress int
	windowSwitching  bool
	blockType        int // 0=长块 1=起始块 2=短块 3=结束块
	mixedBlock       bool
	tableSelect      [3]int
	subblockGain     [3]int
	regionCount      [2]int // scalefactor band 计数;切换块为隐式值(短块 8、混合块等 7),255 表示到末尾
	preflag          int
	scalefacScale    int
	count1Table      int
}

// shape 返回 scalefactor band 布局:只有 block_type 2 可能是短块或混合块,
// 起始/结束块置了 mixed_block_flag 也按长块排列。
func (gi *granuleInfo) shape() blockShape {
	if !gi.windowSwitching || gi.blockType != 2 {
		return shapeLong
	}
	if gi.mixedBlock {
		return shapeMixed
	}
	return shapeShort
}

type sideInfo struct {
	mainDataBegin int
	scfsi         [2][4]int         // 仅 MPEG-1;置位组复用 granule 0 的 scalefactor
	gr            [2][2]granuleInfo // [granule][声道];LSF 只用 granule 0,单声道只用声道 0
}

// parseSideInfo 把一帧的 side information 解析进 si;未使用的槽位保持零值。
func parseSideInfo(h *frameHeader, data []byte, si *sideInfo) error {
	if len(data) < h.sideInfoSize {
		return fmt.Errorf("gl3d: truncated side information: %d < %d", len(data), h.sideInfoSize)
	}
	*si = sideInfo{}
	br := &bitReader{data: data[:h.sideInfoSize]}
	var err error
	read := func(n int) int {
		if err != nil {
			return 0
		}
		var v int
		v, err = br.read(n)
		return v
	}

	nch := h.channels()
	lsf := h.version.lsf()
	if lsf {
		si.mainDataBegin = read(8)
		if nch == 1 {
			read(1)
		} else {
			read(2)
		}
	} else {
		si.mainDataBegin = read(9)
		if nch == 1 {
			read(5)
		} else {
			read(3)
		}
		for ch := 0; ch < nch; ch++ {
			for i := 0; i < 4; i++ {
				si.scfsi[ch][i] = read(1)
			}
		}
	}

	ngr := 2
	if lsf {
		ngr = 1
	}
	for g := 0; g < ngr; g++ {
		for ch := 0; ch < nch; ch++ {
			gi := &si.gr[g][ch]
			gi.part2_3Length = read(12)
			gi.bigValues = read(9)
			gi.globalGain = read(8)
			if lsf {
				gi.scalefacCompress = read(9)
			} else {
				gi.scalefacCompress = read(4)
			}
			gi.windowSwitching = read(1) != 0
			if gi.windowSwitching {
				gi.blockType = read(2)
				gi.mixedBlock = read(1) != 0
				gi.tableSelect[0] = read(5)
				gi.tableSelect[1] = read(5)
				for i := 0; i < 3; i++ {
					gi.subblockGain[i] = read(3)
				}
				if gi.blockType == 2 && !gi.mixedBlock {
					gi.regionCount = [2]int{8, 255}
				} else {
					gi.regionCount = [2]int{7, 255}
				}
			} else {
				for i := 0; i < 3; i++ {
					gi.tableSelect[i] = read(5)
				}
				gi.regionCount[0] = read(4)
				gi.regionCount[1] = read(3)
			}
			if lsf {
				// LSF intensity 右声道使用另一套 scalefac_compress 编码,
				// 不派生 preflag;只有普通声道的最后一组编码带 preflag。
				intensityR := h.channelMode == modeJointStereo && h.modeExt&1 != 0 && ch == 1
				if gi.scalefacCompress >= 500 && !intensityR {
					gi.preflag = 1
				}
			} else {
				gi.preflag = read(1)
			}
			gi.scalefacScale = read(1)
			gi.count1Table = read(1)
		}
	}
	if err != nil {
		return err
	}
	for g := 0; g < ngr; g++ {
		for ch := 0; ch < nch; ch++ {
			gi := &si.gr[g][ch]
			// big_values 以线对计数,一 granule 576 条线对应上限 288
			if gi.bigValues > 288 {
				return &CorruptError{Frame: -1, Detail: "big_values exceeds 288"}
			}
			if gi.windowSwitching && gi.blockType == 0 {
				return &CorruptError{Frame: -1, Detail: "block_type 0 with window_switching"}
			}
			if h.version == MPEG25 && h.srIdx == 2 && gi.windowSwitching && gi.blockType == 2 && gi.mixedBlock {
				// 现有 8 kHz 混合块带表的长区与 IMDCT 长区不一致,重排会重复/
				// 丢失谱线。在整个布局得到验证前明确拒绝,不能静默输出错误音频。
				return &UnsupportedError{Feature: "MPEG-2.5 8 kHz mixed blocks"}
			}
		}
	}
	return nil
}
