package gl3d

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"testing"
)

// big_values 区越过 part2_3_length:保留界内完整解出的谱线,余下置零,读位置停在 granule 结尾。
func TestDecodeSpectraHuffmanOverrunLeavesConcealedSpectrum(t *testing.T) {
	lines := []int{1, 1, 1, 0, 0, 1, 1, 1}
	var w bitWriter
	cut := 0
	for i := 0; i < len(lines); i += 2 {
		writePairT(t, &w, huffBigTables[1], lines[i], lines[i+1])
		if i == 2 {
			cut = w.nbits
		}
	}
	h := &frameHeader{version: mpeg1, srIdx: 0}
	run := func(bigValues int, stale float64) (granuleSpectrum, int, error) {
		si := &sideInfo{}
		si.gr[0][0] = granuleInfo{part2_3Length: cut, bigValues: bigValues, globalGain: 210,
			tableSelect: [3]int{1, 1, 1}, regionCount: [2]int{7, 7}}
		br := &bitReader{data: append(bytes.Clone(w.bytes), make([]byte, 8)...)}
		var gs granuleSpectrum
		gs.xr[100] = stale // 上一 granule 的残留必须被清掉
		err := decodeSpectra(h, si, 0, 0, br, bandMapFor(mpeg1, 0, shapeLong), shapeLong, make([]int, 40), false, &gs)
		return gs, br.pos, err
	}
	// 期望值:只有前两对、part2_3_length 恰好容下它们时的解码结果
	want, _, err := run(2, 0)
	if err != nil || want.xr[0] == 0 || want.xr[4] != 0 {
		t.Fatalf("reference granule: err=%v xr[0]=%v xr[4]=%v", err, want.xr[0], want.xr[4])
	}
	gs, pos, err := run(len(lines)/2, 1)
	var c *CorruptError
	if !errors.As(err, &c) {
		t.Fatalf("err=%v, want CorruptError", err)
	}
	if pos != cut {
		t.Fatalf("read position %d, want the granule end %d", pos, cut)
	}
	if gs.xr != want.xr {
		t.Fatalf("spectrum %v differs from the first two pairs decoded exactly", gs.xr[:8])
	}
}

// scalefactor 越过 part2_3_length:整个 granule 置零,scalefactor 清零,读位置停在 granule 结尾。
func TestDecodeSpectraScalefactorOverrunZeroesGranule(t *testing.T) {
	h := &frameHeader{version: mpeg1, srIdx: 0}
	si := &sideInfo{}
	si.gr[0][0] = granuleInfo{part2_3Length: 10, bigValues: 0, globalGain: 210, scalefacCompress: 15,
		regionCount: [2]int{7, 7}} // slen (4, 3):scalefactor 共 74 位
	for _, intensityR := range []bool{false, true} {
		br := &bitReader{data: bytes.Repeat([]byte{0xA5}, 64)}
		var gs granuleSpectrum
		gs.xr[0] = 1 // 上一 granule 的残留必须被清掉
		gs.ist[0], gs.hasIst = 3, !intensityR
		err := decodeSpectra(h, si, 0, 0, br, bandMapFor(mpeg1, 0, shapeLong), shapeLong, make([]int, 40), intensityR, &gs)
		var c *CorruptError
		if !errors.As(err, &c) {
			t.Fatalf("intensityR=%v: err=%v, want CorruptError", intensityR, err)
		}
		if br.pos != 10 {
			t.Fatalf("intensityR=%v: read position %d, want the granule end 10", intensityR, br.pos)
		}
		if gs.xr != [576]float64{} || gs.scf != [40]int{} || gs.transmitted != 0 {
			t.Fatalf("intensityR=%v: granule not zeroed: scf %v transmitted %d", intensityR, gs.scf, gs.transmitted)
		}
		if gs.ist != [40]int{} || gs.hasIst != intensityR {
			t.Fatalf("intensityR=%v: ist %v hasIst %v, want ist cleared and hasIst following intensityR", intensityR, gs.ist, gs.hasIst)
		}
	}
}

// concealCase 对 data 分别以严格、宽松模式解码:严格模式必须以第 frame 帧的 CorruptError
// 结束;宽松模式必须成功,第一处异常是第 frame 帧(偏移为帧起点)的 corrupt,输出全部有限。
func concealCase(t *testing.T, data []byte, frame int, at int) (*Decoder, []float64) {
	t.Helper()
	_, _, err := decodeMode(t, bytes.NewReader(data), Strict())
	var c *CorruptError
	if !errors.As(err, &c) || c.Frame != int64(frame) {
		t.Fatalf("strict: err=%v, want CorruptError at frame %d", err, frame)
	}
	d, pcm, err := decodeMode(t, bytes.NewReader(data), lenient())
	if err != nil {
		t.Fatalf("lenient: %v", err)
	}
	r := d.Report()
	if want := (Issue{Kind: IssueCorrupt, Frame: int64(frame), Offset: int64(at)}); r.First == nil || *r.First != want {
		t.Fatalf("lenient first issue %+v, want %+v", r.First, want)
	}
	for i, v := range pcm {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("sample %d is %v", i, v)
		}
	}
	return d, pcm
}

// CRC 不符:宽松模式照常解码,输出与完好的流相同。
func TestConcealCRCMismatch(t *testing.T) {
	data := readCorpus(t, "pluck-stereo-32k-cbr96-crc-lame.mp3")
	_, intact, err := decodeMode(t, bytes.NewReader(data), lenient())
	if err != nil {
		t.Fatal(err)
	}
	starts, _ := audioFrames(t, data)
	bad := bytes.Clone(data)
	bad[starts[10]+4] ^= 0xFF // 存储的 CRC
	d, pcm := concealCase(t, bad, 10, starts[10])
	if r := d.Report(); r.CRCErrors != 1 || r.DroppedFrames != 0 || r.ConcealedGranules != 0 {
		t.Fatalf("report %+v, want exactly one CRC error", r)
	}
	if !slices.Equal(pcm, intact) {
		t.Fatal("output differs from the intact stream")
	}
}

// side info 非法:宽松模式丢弃这一帧,main data 仍进入 bit reservoir;再过一帧,解码器状态
// 与完好的流重合,此后的输出与完好的流晚一帧的输出逐字节相同。
func TestConcealInvalidSideInfo(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	_, intact, err := decodeMode(t, bytes.NewReader(data), Strict())
	if err != nil {
		t.Fatal(err)
	}
	starts, _ := audioFrames(t, data)
	bad := bytes.Clone(data)
	setBits(bad[starts[10]+4:], 32, 9, 511) // gr0/ch0 的 big_values 超过 288
	d, pcm := concealCase(t, bad, 10, starts[10])
	const spf = 1152 * 2
	if r := d.Report(); r.DroppedFrames != 1 || r.AudioFrames != int64(len(starts)-1) {
		t.Fatalf("report %+v, want one dropped frame and %d decoded", r, len(starts)-1)
	}
	if !slices.Equal(pcm[:10*spf], intact[:10*spf]) {
		t.Fatal("output before the dropped frame differs")
	}
	if !slices.Equal(pcm[11*spf:], intact[12*spf:]) {
		t.Fatal("output from the second frame after the dropped one differs from the intact stream")
	}
}

// 谱数据损坏:宽松模式把出错的 granule 余下的谱线置零,帧数不变。
func TestConcealSpectralData(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	_, intact, err := decodeMode(t, bytes.NewReader(data), Strict())
	if err != nil {
		t.Fatal(err)
	}
	starts, _ := audioFrames(t, data)
	bad := bytes.Clone(data)
	setBits(bad[starts[10]+4:], 20, 12, 1) // gr0/ch0 的 part2_3_length 只剩 1 位
	d, pcm := concealCase(t, bad, 10, starts[10])
	if r := d.Report(); r.ConcealedGranules < 1 || r.DroppedFrames != 0 || r.AudioFrames != int64(len(starts)) {
		t.Fatalf("report %+v, want concealed granules and every frame decoded", r)
	}
	if len(pcm) != len(intact) || !slices.Equal(pcm[:10*1152*2], intact[:10*1152*2]) {
		t.Fatal("output length or the output before the damaged frame differs")
	}
}

// intensity stereo 的右声道 scalefactor 出错被置零后没有位置可用:各带按 intensity 界限以下
// 处理——不做 MS 时左声道原样、右声道为零,做 MS 时两声道同为左声道的 1/√2。
func TestProcessStereoWithoutTransmittedScalefactors(t *testing.T) {
	h := &frameHeader{version: mpeg1, channelMode: modeJointStereo}
	for _, tc := range []struct {
		name         string
		modeExt      int
		wantL, wantR float64
	}{
		{"intensity", 1, 1, 0},
		{"intensity with MS", 3, 1 / math.Sqrt2, 1 / math.Sqrt2},
	} {
		h.modeExt = tc.modeExt
		var l, r [576]float64
		for i := range l {
			l[i] = 1
		}
		processStereo(h, &granuleInfo{}, bandMapFor(mpeg1, 0, shapeLong), make([]int, 40), 0, &l, &r)
		for i := range l {
			if l[i] != tc.wantL || r[i] != tc.wantR {
				t.Fatalf("%s: line %d = (%v, %v), want (%v, %v)", tc.name, i, l[i], r[i], tc.wantL, tc.wantR)
			}
		}
	}
}

// intensity 立体声帧的右声道 scalefactor 越过 part2_3_length:宽松模式记入报告,整个 granule
// 置零后接着解码,不因没有 intensity 位置而越界。
func TestConcealIntensityStereoRightScalefactors(t *testing.T) {
	body := make([]byte, 400)
	setBits(body, 79, 12, 1)  // gr0/ch1 的 part2_3_length 只剩 1 位
	setBits(body, 108, 4, 15) // 它的 scalefactor 要 74 位
	// 帧头须是 MPEG-1(side info 位置按它算)、joint stereo 且 intensity 置位
	const sel = 1
	h, err := parseFrameHeader(fuzzFrameHeaders[sel])
	if err != nil || h.version != mpeg1 || h.channelMode != modeJointStereo || h.modeExt&1 == 0 {
		t.Fatalf("fuzz header %d: %+v (err=%v), want MPEG-1 joint stereo with intensity", sel, h, err)
	}
	d, _ := concealCase(t, buildFrames(sel, body), 0, 0)
	if r := d.Report(); r.ConcealedGranules != 1 || r.DroppedFrames != 0 || r.AudioFrames != 2 {
		t.Fatalf("report %+v, want one concealed granule and both frames decoded", r)
	}
}
