package gl3d

import (
	"testing"
)

// bitWriter MSB 优先地写入位,用于构造 side info 测试数据。
type bitWriter struct {
	bytes []byte
	nbits int
}

func (w *bitWriter) write(v, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.nbits%8 == 0 {
			w.bytes = append(w.bytes, 0)
		}
		if v>>i&1 != 0 {
			w.bytes[w.nbits/8] |= 1 << (7 - w.nbits%8)
		}
		w.nbits++
	}
}

func TestBitReader(t *testing.T) {
	b := &bitReader{data: []byte{0b10110011, 0b01000001}}
	if v, err := b.read(1); err != nil || v != 1 {
		t.Fatalf("read(1)=%d,%v", v, err)
	}
	if v, err := b.read(3); err != nil || v != 0b011 {
		t.Fatalf("read(3)=%d,%v", v, err)
	}
	if v, err := b.read(9); err != nil || v != 0b01101000 {
		t.Fatalf("read(9)=%09b,%v want 01101000", v, err)
	}
	if b.bitsLeft() != 3 {
		t.Fatalf("bitsLeft=%d, want 3", b.bitsLeft())
	}
	if v, err := b.read(3); err != nil || v != 1 {
		t.Fatalf("read(3)=%d,%v", v, err)
	}
	if _, err := b.read(1); err == nil {
		t.Fatal("want EOF error")
	}
	if _, err := b.read(33); err == nil {
		t.Fatal("want error for n>32")
	}
}

func TestParseSideInfoMPEG1Stereo(t *testing.T) {
	var w bitWriter
	w.write(0x155, 9) // main_data_begin
	w.write(5, 3)     // private
	w.write(0b1010, 4)
	w.write(0b0101, 4) // scfsi ch0/ch1
	for g := 0; g < 2; g++ {
		for ch := 0; ch < 2; ch++ {
			w.write(0xABC, 12) // part2_3_length
			w.write(0x120, 9)  // big_values(288 上限)
			w.write(0xAB, 8)   // global_gain
			w.write(9, 4)      // scalefac_compress
			w.write(0, 1)      // window_switching_flag
			w.write(1, 5)      // table_select x3
			w.write(2, 5)
			w.write(3, 5)
			w.write(5, 4) // region0_count
			w.write(6, 3) // region1_count
			w.write(1, 1) // preflag
			w.write(1, 1) // scalefac_scale
			w.write(1, 1) // count1table_select
		}
	}
	if len(w.bytes) != 32 {
		t.Fatalf("constructed %d bytes, want 32", len(w.bytes))
	}
	h, err := parseFrameHeader(mkHeader(3, 1, 1, 11, 0, 0, 0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var si sideInfo
	if err := parseSideInfo(&h, w.bytes, &si); err != nil {
		t.Fatal(err)
	}
	if si.mainDataBegin != 0x155 {
		t.Fatalf("mainDataBegin=%x", si.mainDataBegin)
	}
	if si.scfsi[0] != [4]int{1, 0, 1, 0} || si.scfsi[1] != [4]int{0, 1, 0, 1} {
		t.Fatalf("scfsi=%v", si.scfsi)
	}
	g := si.gr[0][1]
	if g.part2_3Length != 0xABC || g.bigValues != 0x120 || g.globalGain != 0xAB ||
		g.scalefacCompress != 9 || g.windowSwitching || g.tableSelect != [3]int{1, 2, 3} ||
		g.regionCount != [2]int{5, 6} || g.preflag != 1 || g.scalefacScale != 1 || g.count1Table != 1 {
		t.Fatalf("granule: %+v", g)
	}
}

func TestParseSideInfoMPEG25MonoShortBlocks(t *testing.T) {
	var w bitWriter
	w.write(0x80, 8) // main_data_begin(LSF 8 bit)
	w.write(1, 1)    // private(mono LSF 1 bit)
	w.write(100, 12) // part2_3_length
	w.write(288, 9)  // big_values
	w.write(200, 8)  // global_gain
	w.write(511, 9)  // scalefac_compress(LSF 9 bit)
	w.write(1, 1)    // window_switching
	w.write(2, 2)    // block_type short
	w.write(1, 1)    // mixed_block
	w.write(4, 5)    // table_select x2
	w.write(5, 5)
	w.write(1, 3) // subblock_gain x3
	w.write(2, 3)
	w.write(3, 3)
	w.write(0, 1) // scalefac_scale
	w.write(1, 1) // count1table_select
	if len(w.bytes) != 9 {
		t.Fatalf("constructed %d bytes, want 9", len(w.bytes))
	}
	h, err := parseFrameHeader(mkHeader(0, 1, 1, 4, 0, 0, 3, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var si sideInfo
	if err := parseSideInfo(&h, w.bytes, &si); err != nil {
		t.Fatal(err)
	}
	if si.mainDataBegin != 0x80 {
		t.Fatalf("mainDataBegin=%x", si.mainDataBegin)
	}
	if si.gr[0][1] != (granuleInfo{}) || si.gr[1] != [2]granuleInfo{} {
		t.Fatalf("LSF mono must fill only granule 0 channel 0: %+v", si.gr)
	}
	g := si.gr[0][0]
	if !g.windowSwitching || g.blockType != 2 || !g.mixedBlock ||
		g.tableSelect[0] != 4 || g.tableSelect[1] != 5 || g.subblockGain != [3]int{1, 2, 3} {
		t.Fatalf("granule: %+v", g)
	}
	if g.preflag != 1 {
		t.Fatalf("LSF sfc=511 must derive preflag=1, got %d", g.preflag)
	}
}

func TestParseSideInfoRejects(t *testing.T) {
	h, _ := parseFrameHeader(mkHeader(3, 1, 1, 11, 0, 0, 0, 0, 0))

	// big_values > 288 非法(576 条谱线 / 2)
	var w bitWriter
	w.write(0, 9+3+8) // main_data_begin + private + scfsi
	w.write(100, 12)  // part2_3_length
	w.write(289, 9)   // big_values 超界
	w.write(100, 8)   // global_gain
	w.write(0, 4)     // scalefac_compress
	w.write(0, 1)     // window_switching = 0
	w.write(0, 15)    // table_select x3
	w.write(0, 4)     // region0_count
	w.write(0, 3)     // region1_count
	w.write(0, 3)     // preflag + scalefac_scale + count1table_select
	w.write(0, 59*3)  // 其余 granule x 声道
	if len(w.bytes) != 32 {
		t.Fatalf("constructed %d bytes, want 32", len(w.bytes))
	}
	if err := parseSideInfo(&h, w.bytes, &sideInfo{}); err == nil {
		t.Fatal("want error for big_values > 288")
	}

	if err := parseSideInfo(&h, make([]byte, 31), &sideInfo{}); err == nil {
		t.Fatal("want error for truncated side info")
	}
}
