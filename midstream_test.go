package gl3d

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// lameStream 返回首帧是 Xing 标签帧、其后的帧使用 bit reservoir 的 MPEG-1 流,
// 以及各音频帧的起点与一个回指上一帧数据的帧(main_data_begin 非零)的序号。
func lameStream(t *testing.T) (data []byte, starts []int, k int) {
	t.Helper()
	data = readCorpus(t, "pluck-stereo-44k1-abr128-lame.mp3")
	starts, _ = audioFrames(t, data)
	if len(starts) == 0 || starts[0] == 0 {
		t.Fatal("fixture: no tag frame before the audio frames")
	}
	for k = 10; k < len(starts)-10; k++ {
		h, err := parseFrameHeader([4]byte(data[starts[k]:]))
		if err != nil {
			t.Fatal(err)
		}
		si := data[starts[k]+4:]
		if h.hasCRC {
			si = si[2:]
		}
		if int(si[0])<<1|int(si[1])>>7 != 0 { // MPEG-1 的 main_data_begin 占 9 位
			return data, starts, k
		}
	}
	t.Fatal("fixture: no frame uses the bit reservoir")
	return nil, nil, 0
}

// 宽松模式按长度跳过流中间的标签,bit reservoir 保留:即使紧随其后的帧回指标签
// 之前的数据,输出也与去掉标签的流逐字节相同;报告计数并记下第一个标签。
func TestMidstreamTagsLenient(t *testing.T) {
	x, starts, k := lameStream(t)
	_, want, err := decodeMode(t, bytes.NewReader(x), lenient())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		tags []byte
		n    int64
	}{
		{"ID3v2", id3v2Tag(100), 1},
		{"ID3v1", id3v1Tag(), 1},
		{"APEv2 then ID3v1", cat(apeTag(true, [2]string{"Title", "gl3d"}), id3v1Tag()), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := cat(x[:starts[k]], tc.tags, x[starts[k]:])
			d, got, err := decodeMode(t, bytes.NewReader(data), lenient())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatal("PCM differs from the stream without the tags")
			}
			r := d.Report()
			first := Issue{Kind: IssueCorrupt, Frame: int64(k), Offset: int64(starts[k])}
			if r.Clean() || r.MidstreamTags != tc.n || *r.First != first || r.TrailingTags != 0 {
				t.Fatalf("report %+v, first %+v, want %d mid-stream tags and first %+v", r, r.First, tc.n, first)
			}
		})
	}
}

// 流中间的标签帧(拼接处后一个流的开头):严格模式报格式错误;宽松模式整帧跳过,
// 其后的帧不能回指它之前的 main data。
func TestMidstreamTagFrame(t *testing.T) {
	x, starts, k := lameStream(t)
	data := cat(x[:starts[k]], x[:starts[0]], x[starts[k]:]) // 在第 k 帧之前插入一份首帧(标签帧)
	first := Issue{Kind: IssueCorrupt, Frame: int64(k), Offset: int64(starts[k])}

	t.Run("strict", func(t *testing.T) {
		d, _, err := decodeMode(t, bytes.NewReader(data), Strict())
		var c *CorruptError
		if !errors.As(err, &c) || !strings.HasSuffix(c.Detail, " tag frame in mid-stream") || c.Frame != int64(k) {
			t.Fatalf("err=%v, want CorruptError for a tag frame at frame %d", err, k)
		}
		if r := d.Report(); r.First == nil || *r.First != first {
			t.Fatalf("first %+v, want %+v", r.First, first)
		}
	})

	// 标签帧之后是无法识别的数据:宽松模式跳过它,从其后的帧继续
	t.Run("junk after the tag frame", func(t *testing.T) {
		junk := []byte("junk junk junk junk")
		d, _, err := decodeMode(t, bytes.NewReader(cat(x[:starts[k]], x[:starts[0]], junk, x[starts[k]:])), lenient())
		if err != nil {
			t.Fatal(err)
		}
		if r := d.Report(); r.MidstreamTags != 1 || r.SkippedBytes != int64(len(junk)) || r.Resyncs != 1 || r.First == nil || *r.First != first {
			t.Fatalf("report %+v, first %+v, want first %+v", r, r.First, first)
		}
	})

	t.Run("gap flag", func(t *testing.T) { checkGapOnlyAt(t, data, len(starts), k) })

	t.Run("lenient", func(t *testing.T) {
		dx, want, err := decodeMode(t, bytes.NewReader(x), lenient())
		if err != nil {
			t.Fatal(err)
		}
		d, got, err := decodeMode(t, bytes.NewReader(data), lenient())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%d samples, want %d: the tag frame is not audio", len(got), len(want))
		}
		// 第 k 帧之前逐字节相同;第 k 帧回指的数据在断档之前,不再可用,输出随之不同
		nch := dx.Channels()
		at := (k*1152 - (dx.lame.encDelay + decoderDelay)) * nch
		if !slices.Equal(got[:at], want[:at]) {
			t.Fatal("output before the tag frame differs")
		}
		if slices.Equal(got[at:at+1152*nch], want[at:at+1152*nch]) {
			t.Fatal("frame after the tag frame decoded with the main data from before it")
		}
		if r := d.Report(); r.Clean() || r.MidstreamTags != 1 || *r.First != first {
			t.Fatalf("report %+v, first %+v", r, r.First)
		}
	})
}

// checkGapOnlyAt 以宽松模式的分帧层读完 data,断言共 n 个音频帧、只有第 k 帧带断档标记。
func checkGapOnlyAt(t *testing.T, data []byte, n, k int) {
	t.Helper()
	fr := newFramer(bytes.NewReader(data), false)
	if _, err := fr.open(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		af, err := fr.next(context.Background())
		if err == errEndOfAudio {
			if i != n {
				t.Fatalf("%d audio frames, want %d", i, n)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if af.gap != (i == k) {
			t.Fatalf("frame %d: gap=%v", i, af.gap)
		}
	}
}

// 一串标签之后紧跟标签帧:标签与标签帧都在流中间,各算一个;第一处异常是那串标签的
// 起点,断档标记只落在标签帧之后的那一帧。
func TestMidstreamTagRunThenTagFrame(t *testing.T) {
	x, starts, k := lameStream(t)
	data := cat(x[:starts[k]], id3v2Tag(100), x[:starts[0]], x[starts[k]:])

	t.Run("lenient", func(t *testing.T) {
		d, _, err := decodeMode(t, bytes.NewReader(data), lenient())
		if err != nil {
			t.Fatal(err)
		}
		first := Issue{Kind: IssueCorrupt, Frame: int64(k), Offset: int64(starts[k])}
		if r := d.Report(); r.MidstreamTags != 2 || r.First == nil || *r.First != first || r.TrailingTags != 0 {
			t.Fatalf("report %+v, first %+v, want 2 mid-stream tags and first %+v", r, r.First, first)
		}
	})

	t.Run("gap flag", func(t *testing.T) { checkGapOnlyAt(t, data, len(starts), k) })
}

// 流尾的标签帧前有一串标签时,两者都在流中间;跳过它们之后流在帧边界结束,那串标签
// 不能再被记为尾部标签。
func TestMidstreamTagRunThenTagFrameAtTheEnd(t *testing.T) {
	x, starts, _ := lameStream(t)
	d, _, err := decodeMode(t, bytes.NewReader(cat(x, id3v2Tag(100), x[:starts[0]])), lenient())
	if err != nil {
		t.Fatal(err)
	}
	if r := d.Report(); r.MidstreamTags != 2 || r.TrailingTags != 0 {
		t.Fatalf("report %+v, want 2 mid-stream tags and no trailing tags", r)
	}
}

// 流尾的标签帧被截断时,帧体不完整,无从确认它是标签帧:两种模式都按截断记入报告,
// 不计入流中间的标签。
func TestTruncatedTagFrameAtTheEnd(t *testing.T) {
	x, starts, _ := lameStream(t)
	data := cat(x, x[:starts[0]/2])
	for _, m := range []struct {
		name   string
		opts   []Option
		strict bool
	}{{"strict", []Option{Strict()}, true}, {"lenient", []Option{lenient()}, false}} {
		t.Run(m.name, func(t *testing.T) {
			d, _, err := decodeMode(t, bytes.NewReader(data), m.opts...)
			if m.strict {
				var tr *TruncatedError
				if !errors.As(err, &tr) || !strings.Contains(tr.What, "frame body") {
					t.Fatalf("err=%v, want TruncatedError for the frame body", err)
				}
			}
			if r := d.Report(); r.MidstreamTags != 0 || r.First == nil || r.First.Kind != IssueTruncated {
				t.Fatalf("report %+v, first %+v, want no mid-stream tags and a truncation", r, r.First)
			}
		})
	}
}

// 标签帧在帧头(及 CRC)与标识之间不放 side information;音频帧的主数据恰好以
// 标识开头时不算标签帧。
func TestTagFrameMagic(t *testing.T) {
	mono := mkHeader(3, 1, 1, 9, 0, 0, 3, 0, 0)    // MPEG-1 单声道 128k 44.1k:417 B,side info 17 B
	monoCRC := mkHeader(3, 1, 0, 9, 0, 0, 3, 0, 0) // 同上,带 CRC
	build := func(h [4]byte, off int, magic string, poke int) []byte {
		b := make([]byte, 417)
		copy(b, h[:])
		copy(b[off:], magic)
		if poke > 0 {
			b[poke] = 1
		}
		return b
	}
	for _, tc := range []struct {
		name  string
		frame []byte
		want  string
	}{
		{"Info", build(mono, 4+17, "Info", 0), "Info"},
		{"Xing", build(mono, 4+17, "Xing", 0), "Xing"},
		{"VBRI", build(mono, 36, "VBRI", 0), "VBRI"},
		{"Info with CRC", build(monoCRC, 4+17, "Info", 4), "Info"}, // CRC 字段不必为零
		{"Info after side information", build(mono, 4+17, "Info", 10), ""},
		{"VBRI after non-zero bytes", build(mono, 36, "VBRI", 30), ""},
		{"no magic", build(mono, 4+17, "Abcd", 0), ""},
	} {
		h, err := parseFrameHeader([4]byte(tc.frame))
		if err != nil {
			t.Fatal(err)
		}
		if got := tagFrameMagic(tc.frame, h); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// 帧小得容不下 VBRI 的定长部分(26 字节)时,帧头后 32 字节处的 "VBRI" 只是音频数据:
// 首帧与流中间的帧按同一条规则判断,不会一处当标签帧、另一处当音频。
func TestTinyFrameHoldingMagicBytesIsAudio(t *testing.T) {
	h := mkHeader(2, 1, 1, 2, 0, 0, 3, 0, 0) // MPEG-2 单声道 16 kbps 22.05 kHz:52 字节,36+26 > 52
	frame := make([]byte, 52)
	copy(frame, h[:])
	copy(frame[36:], "VBRI")
	data := bytes.Repeat(frame, 4)
	for _, m := range []struct {
		name string
		opts []Option
	}{{"strict", []Option{Strict()}}, {"lenient", []Option{lenient()}}} {
		t.Run(m.name, func(t *testing.T) {
			d, _, err := decodeMode(t, bytes.NewReader(data), m.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if r := d.Report(); !r.Clean() || r.AudioFrames != 4 || r.MidstreamTags != 0 || d.vbri != nil {
				t.Fatalf("report %+v, vbri %+v: want 4 audio frames and no tag frame", r, d.vbri)
			}
		})
	}
}
