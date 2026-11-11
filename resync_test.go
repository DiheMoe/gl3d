package gl3d

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// noSync 返回 n 字节确定性的填充,不含 0xFF,重新同步不会在其中找到帧。
func noSync(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7%200 + 1)
	}
	return b
}

// 音频之后的填充、垃圾、不完整的帧头或帧(其后可接 ID3v1):严格模式报错,首个异常与
// 宽松模式相同。宽松模式交出所有完整的帧,跳过的字节、截断与尾部标签记入报告。
func TestLenientTrailingData(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	n := len(starts)
	last := int64(starts[n-1])
	_, want := decodeAll(t, bytes.NewReader(shine))
	cases := []struct {
		name      string
		data      []byte
		frames    int
		skipped   int64
		resyncs   int64
		truncated bool
		tags      TagSet
		first     Issue
	}{
		{"zeros", cat(shine, make([]byte, 256)), n, 256, 0, false, 0, Issue{Kind: IssueCorrupt, Frame: int64(n - 1), Offset: last}},
		{"junk", cat(shine, noSync(1024)), n, 1024, 0, false, 0, Issue{Kind: IssueCorrupt, Frame: int64(n - 1), Offset: last}},
		{"half a frame header", cat(shine, []byte{0xFF, 0xFB}), n, 2, 0, true, 0, Issue{Kind: IssueTruncated, Frame: int64(n - 1), Offset: int64(len(shine))}},
		{"last frame cut", shine[:len(shine)-100], n - 1, int64(len(shine) - 100 - starts[n-1]), 0, true, 0, Issue{Kind: IssueTruncated, Frame: int64(n - 1), Offset: last}},
		{"last frame cut then ID3v1", cat(shine[:starts[n-1]+200], id3v1Tag()), n - 1, 200, 1, true, TagID3v1, Issue{Kind: IssueTruncated, Frame: int64(n - 1), Offset: last}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds, _, err := decodeMode(t, bytes.NewReader(tc.data), Strict())
			if err == nil {
				t.Fatal("strict mode accepted the stream")
			}
			if r := ds.Report(); r.First == nil || *r.First != tc.first {
				t.Fatalf("strict: err=%v, first %+v, want first %+v", err, r.First, tc.first)
			}
			d, got, err := decodeMode(t, bytes.NewReader(tc.data), lenient())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want[:tc.frames*1152*2]) {
				t.Fatalf("PCM: %d samples, want the first %d frames of the clean stream", len(got), tc.frames)
			}
			r := d.Report()
			if r.SkippedBytes != tc.skipped || r.Truncated != tc.truncated || r.Resyncs != tc.resyncs || r.TrailingTags != tc.tags || r.DroppedFrames != 0 || r.First == nil || *r.First != tc.first {
				t.Fatalf("report %+v, first %+v; want skipped %d, resyncs %d, truncated %v, tags %q, first %+v", r, r.First, tc.skipped, tc.resyncs, tc.truncated, tc.tags, tc.first)
			}
		})
	}
}

// 帧之间夹着垃圾:宽松模式跳过它,从其后的帧继续,两种读取方式结果相同。垃圾之前的
// 输出逐字节相同,重新同步后的第一帧不能回指垃圾之前的数据。
func TestLenientResyncAfterJunk(t *testing.T) {
	x, starts, k := lameStream(t)
	dx, want, err := decodeMode(t, bytes.NewReader(x), lenient())
	if err != nil {
		t.Fatal(err)
	}
	data := cat(x[:starts[k]], noSync(500), x[starts[k]:])
	if _, _, err := decodeMode(t, bytes.NewReader(data), Strict()); err == nil {
		t.Fatal("strict mode accepted junk between frames")
	}
	nch := dx.Channels()
	at := (k*1152 - (dx.lame.encDelay + decoderDelay)) * nch
	first := Issue{Kind: IssueCorrupt, Frame: int64(k - 1), Offset: int64(starts[k-1])}
	t.Run("gap flag", func(t *testing.T) { checkGapOnlyAt(t, data, len(starts), k) })
	for name, r := range map[string]io.Reader{
		"bytes":   bytes.NewReader(data),
		"onebyte": &oneByteReader{r: bytes.NewReader(data)},
	} {
		t.Run(name, func(t *testing.T) {
			d, got, err := decodeMode(t, r, lenient())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("%d samples, want %d", len(got), len(want))
			}
			if !slices.Equal(got[:at], want[:at]) {
				t.Fatal("output before the junk differs")
			}
			if slices.Equal(got[at:at+1152*nch], want[at:at+1152*nch]) {
				t.Fatal("frame after the junk decoded with main data from before it")
			}
			rep := d.Report()
			if rep.SkippedBytes != 500 || rep.Resyncs != 1 || rep.DroppedFrames != 0 || rep.First == nil || *rep.First != first {
				t.Fatalf("report %+v, first %+v, want first %+v", rep, rep.First, first)
			}
		})
	}
}

// 一串帧之后各夹着一个垃圾字节,所以帧长处落在垃圾上,按帧长精确确认会把这一串完好的帧全部
// 丢掉。重新同步还接受帧后 resyncSlack 字节内出现同格式帧头的候选,这些帧一帧不少地解出。
// shine 不用 bit reservoir,所以输出与完好的流逐字节相同。
func TestLenientResyncKeepsFramesFollowedByJunk(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	n := len(starts)
	_, want := decodeAll(t, bytes.NewReader(shine))
	const from, to = 50, 70 // 第 [from, to) 帧之后各夹一个字节
	data := bytes.Clone(shine[:starts[from]])
	for k := from; k < to; k++ {
		data = append(append(data, shine[starts[k]:starts[k+1]]...), 0)
	}
	data = append(data, shine[starts[to]:]...)
	if _, _, err := decodeMode(t, bytes.NewReader(data), Strict()); err == nil {
		t.Fatal("strict mode accepted junk between frames")
	}
	d, got, err := decodeMode(t, bytes.NewReader(data), lenient())
	if err != nil {
		t.Fatal(err)
	}
	r := d.Report()
	first := Issue{Kind: IssueCorrupt, Frame: from, Offset: int64(starts[from])}
	if !slices.Equal(got, want) || r.AudioFrames != int64(n) || r.SkippedBytes != to-from || r.Resyncs != to-from ||
		r.DroppedFrames != 0 || r.First == nil || *r.First != first {
		t.Fatalf("%d frames (want %d), report %+v, first %+v; want every frame, %d bytes skipped, %d resyncs, first %+v",
			r.AudioFrames, n, r, r.First, to-from, to-from, first)
	}
}

// 松弛只在重新同步里用,且到 resyncSlack 为止,也就是帧后第 resyncSlack 字节处的帧头
// 还算,再远一字节不算。精确的规则(slack 为零)不接受帧头前的任何垃圾。
func TestCandidateSlackBoundary(t *testing.T) {
	h := mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0) // 417 B
	hdr, err := parseFrameHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	f := newFramer(bytes.NewReader(nil), false)
	f.header = hdr
	for _, tc := range []struct {
		junk         int
		exact, slack bool
	}{
		{0, true, true},
		{1, false, true},
		{resyncSlack, false, true},
		{resyncSlack + 1, false, false},
	} {
		b := cat(repeatFrames(h, 417, 1), make([]byte, tc.junk), repeatFrames(h, 417, 1))
		if got := f.candidateAt(b, 0); got != tc.exact {
			t.Errorf("%d junk bytes, exact rule: %v, want %v", tc.junk, got, tc.exact)
		}
		if got := f.candidateAt(b, resyncSlack); got != tc.slack {
			t.Errorf("%d junk bytes, with slack: %v, want %v", tc.junk, got, tc.slack)
		}
	}
}

// 一帧中间丢了字节,所以它的帧长指向下一帧内部,帧内找得到下一帧的起点。宽松模式丢弃
// 这一帧,从下一帧继续,两帧之后输出与完好的流逐字节相同。严格模式报同步丢失。
// 损坏的是倒数第二帧时,最后一帧作为候选,它的帧长处是 ID3v1 或流结束同样成立。损坏的
// 帧仍被丢弃,而不是连同最后一帧的开头一起当作完好的帧交出。
func TestLenientDropsDamagedFrame(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	n := len(starts)
	_, want := decodeAll(t, bytes.NewReader(shine))
	for _, tc := range []struct {
		name string
		k    int
		tail []byte
		tags TagSet
	}{
		{"mid-stream", 100, nil, 0},
		{"candidate followed by an ID3v1 tag", n - 2, id3v1Tag(), TagID3v1},
		{"candidate at the end of the stream", n - 2, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := tc.k
			cut := starts[k] + 300
			data := cat(shine[:cut], shine[cut+50:]) // 第 k 帧中间删去 50 字节
			if k == 100 {
				if _, _, err := decodeMode(t, bytes.NewReader(data), Strict()); err == nil {
					t.Fatal("strict mode accepted a damaged frame")
				}
			}
			d, got, err := decodeMode(t, bytes.NewReader(cat(data, tc.tail)), lenient())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want)-1152*2 {
				t.Fatalf("%d samples, want %d (one frame dropped)", len(got), len(want)-1152*2)
			}
			if !slices.Equal(got[:k*1152*2], want[:k*1152*2]) {
				t.Fatal("output before the damaged frame differs")
			}
			r := d.Report()
			first := Issue{Kind: IssueCorrupt, Frame: int64(k), Offset: int64(starts[k])}
			skipped := int64(starts[k+1] - starts[k] - 50)
			if r.DroppedFrames != 1 || r.Resyncs != 1 || r.SkippedBytes != skipped || r.TrailingTags != tc.tags || r.First == nil || *r.First != first {
				t.Fatalf("report %+v, first %+v; want one dropped frame, %d bytes skipped, tags %q, first %+v", r, r.First, skipped, tc.tags, first)
			}
			if k != 100 {
				return
			}
			tail := 80 * 1152 * 2
			if !slices.Equal(got[len(got)-tail:], want[len(want)-tail:]) {
				t.Fatal("output does not recover after the dropped frame")
			}
			// shine 的帧不回指 bit reservoir,输出看不出断档标记。丢弃一帧之后交出的第一帧须带断档标记
			checkGapOnlyAt(t, data, len(starts)-1, k)
		})
	}
}

// 流中间夹着格式不同的帧:严格模式报不支持。宽松模式按垃圾跳过,从其后同格式的帧继续。
func TestLenientSkipsOtherFormat(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	other := repeatFrames(mkHeader(3, 1, 1, 11, 1, 0, 0, 0, 0), 576, 2) // 192 kbps @ 48 kHz:576 B
	data := cat(shine, other, shine)
	if _, _, err := decodeMode(t, bytes.NewReader(data), Strict()); errClass(err) != "unsupported" {
		t.Fatalf("strict: err=%v, want unsupported", err)
	}
	_, one := decodeAll(t, bytes.NewReader(shine))
	d, got, err := decodeMode(t, bytes.NewReader(data), lenient())
	if err != nil {
		t.Fatal(err)
	}
	// 第二份从断档之后开始,IMDCT 重叠与合成滤波器状态接着第一份的结尾,开头与单独解码的
	// one 不同,所以只比较长度与第一份
	if len(got) != 2*len(one) || !slices.Equal(got[:len(one)], one) {
		t.Fatalf("%d samples, want %d with the first copy unchanged", len(got), 2*len(one))
	}
	r := d.Report()
	first := Issue{Kind: IssueUnsupported, Frame: int64(len(starts)), Offset: int64(len(shine))}
	if r.SkippedBytes != int64(len(other)) || r.Resyncs != 1 || r.First == nil || *r.First != first {
		t.Fatalf("report %+v, first %+v, want first %+v", r, r.First, first)
	}
}

// 重新同步只在 mayStart 里的字节上检查候选,所以标签的首字母 T、I、A、L 都要在其中,
// 垃圾之后的标签才认得出。标签之后是流结束时记为尾部标签,是帧时记为流中间的标签。
func TestResyncStopsAtEveryTagKind(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	n := len(starts)
	_, one := decodeAll(t, bytes.NewReader(shine))
	_, two := decodeAll(t, bytes.NewReader(cat(shine, shine)))
	junk := noSync(300)
	first := Issue{Kind: IssueCorrupt, Frame: int64(n - 1), Offset: int64(starts[n-1])} // 最后一帧之后同步丢失
	for _, tc := range []struct {
		name  string
		tags  []byte
		kinds TagSet
		count int64 // 标签个数
	}{
		{"ID3v2", id3v2Tag(100), TagID3v2, 1},
		{"APEv2 with header", apeTag(true, [2]string{"Title", "gl3d"}), TagAPEv2, 1},
		{"Lyrics3 v2 then ID3v1", cat(lyrics3v2("la"), id3v1Tag()), TagLyrics3 | TagID3v1, 2},
		{"ID3v1", id3v1Tag(), TagID3v1, 1},
	} {
		t.Run(tc.name+"/end of stream", func(t *testing.T) {
			d, got, err := decodeMode(t, bytes.NewReader(cat(shine, junk, tc.tags)), lenient())
			if err != nil {
				t.Fatal(err)
			}
			r := d.Report()
			if !slices.Equal(got, one) || r.SkippedBytes != int64(len(junk)) || r.Resyncs != 1 ||
				r.TrailingTags != tc.kinds || r.MidstreamTags != 0 || r.First == nil || *r.First != first {
				t.Fatalf("%d samples (want %d), report %+v (tags %q), first %+v; want %d bytes skipped, one resync, trailing tags %q, first %+v",
					len(got), len(one), r, r.TrailingTags, r.First, len(junk), tc.kinds, first)
			}
		})
		t.Run(tc.name+"/before frames", func(t *testing.T) {
			d, got, err := decodeMode(t, bytes.NewReader(cat(shine, junk, tc.tags, shine)), lenient())
			if err != nil {
				t.Fatal(err)
			}
			r := d.Report()
			if !slices.Equal(got, two) || r.SkippedBytes != int64(len(junk)) || r.Resyncs != 1 ||
				r.TrailingTags != 0 || r.MidstreamTags != tc.count || r.First == nil || *r.First != first {
				t.Fatalf("%d samples (want %d), report %+v, first %+v; want %d bytes skipped, one resync, %d mid-stream tags, first %+v",
					len(got), len(two), r, r.First, len(junk), tc.count, first)
			}
		})
	}
}

// 标识认得出、却读不下去的标签(没有结束标记的 Lyrics3、长度字段不合理的 APEv2)一个
// 字节也没有读过就失败:宽松模式把它整个当作未知数据跳过,不能在原处反复停下。
func TestLenientUnreadableTags(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	frames, end := len(starts), int64(len(shine))
	shortHeader := apeTag(true, [2]string{"Title", "gl3d"})
	binary.LittleEndian.PutUint32(shortHeader[12:], 10) // 头部声明的长度小于头部本身
	emptyWrongSize := apeTag(false)
	binary.LittleEndian.PutUint32(emptyWrongSize[12:], 99) // 没有条目,长度却不是 32
	for _, tc := range []struct {
		name string
		tail []byte
		kind IssueKind
	}{
		{"Lyrics3 v1 without an end marker", []byte("LYRICSBEGINla la"), IssueTruncated},
		{"Lyrics3 v1 over 5100 bytes", lyrics3v1(strings.Repeat("a", lyrics3v1Max+1)), IssueCorrupt},
		{"APEv2 header shorter than itself", shortHeader, IssueCorrupt},
		{"APEv2 without items and the wrong size", emptyWrongSize, IssueCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 原地打转的实现在这里耗到期限,以 context 错误结束
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			fr := newFramer(bytes.NewReader(cat(shine, tc.tail)), false)
			if _, err := fr.open(ctx); err != nil {
				t.Fatal(err)
			}
			got := 0
			for {
				_, err := fr.next(ctx)
				if err == errEndOfAudio {
					break
				}
				if err != nil {
					t.Fatalf("after %d frames: %v", got, err)
				}
				got++
			}
			first := Issue{Kind: tc.kind, Frame: int64(frames), Offset: end}
			if got != frames || fr.rep.SkippedBytes != int64(len(tc.tail)) || fr.rep.TrailingTags != 0 || fr.rep.First == nil || *fr.rep.First != first {
				t.Fatalf("%d frames, report %+v, first %+v; want %d frames, %d bytes skipped, first %+v", got, fr.rep, fr.rep.First, frames, len(tc.tail), first)
			}
		})
	}
}

// 一串标签之后不是重新同步可以接受的帧(帧头有效却确认不了,或是垃圾):标签不在流尾。
// 严格模式报标签之后的未知数据,指向最后一个标签的起点。宽松模式记下同一处异常,把这串
// 标签计为流中间的标签。其后的数据交给帧的规则,完好的帧照常交出,垃圾从后面重新同步。
func TestTagRunBeforeUnconfirmedData(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	n := len(starts)
	const k = 50
	first := Issue{Kind: IssueCorrupt, Frame: k, Offset: int64(starts[k])}

	t.Run("strict", func(t *testing.T) {
		// 帧头有效,帧体只有 100 字节。帧头本身不足以说明标签在流中间
		data := cat(shine[:starts[k]], id3v1Tag(), shine[starts[k]:starts[k]+100])
		d, _, err := decodeMode(t, bytes.NewReader(data), Strict())
		var c *CorruptError
		if !errors.As(err, &c) || c.Detail != "unknown data after ID3v1 tag" || c.Frame != k {
			t.Fatalf("err=%v, want CorruptError %q at frame %d", err, "unknown data after ID3v1 tag", k)
		}
		if r := d.Report(); r.First == nil || *r.First != first || r.MidstreamTags != 0 || d.DecodedFrames() != k*1152 {
			t.Fatalf("report %+v, first %+v, decoded %d; want first %+v and the %d frames before the tag", r, r.First, d.DecodedFrames(), first, k)
		}
	})

	// 标签之后的最后一帧完好、其后是垃圾:这一帧与没有标签时一样交出,不能被当作垃圾丢掉
	t.Run("lenient, intact frame then junk", func(t *testing.T) {
		last := shine[starts[n-1]:]
		junk := noSync(100)
		_, want := decodeAll(t, bytes.NewReader(cat(shine, last)))
		d, got, err := decodeMode(t, bytes.NewReader(cat(shine, id3v1Tag(), last, junk)), lenient())
		if err != nil {
			t.Fatal(err)
		}
		atTag := Issue{Kind: IssueCorrupt, Frame: int64(n), Offset: int64(len(shine))}
		r := d.Report()
		if !slices.Equal(got, want) || r.AudioFrames != int64(n+1) || r.MidstreamTags != 1 || r.SkippedBytes != int64(len(junk)) ||
			r.Resyncs != 0 || r.TrailingTags != 0 || r.First == nil || *r.First != atTag {
			t.Fatalf("%d samples (want %d), report %+v, first %+v; want %d frames, one mid-stream tag, %d bytes skipped, first %+v",
				len(got), len(want), r, r.First, n+1, len(junk), atTag)
		}
	})

	// 标签之后是垃圾,垃圾之后又是帧:垃圾跳过,其后的帧全部解出,标签算作流中间的
	t.Run("lenient, junk then frames", func(t *testing.T) {
		junk := noSync(300)
		_, want := decodeAll(t, bytes.NewReader(shine))
		data := cat(shine[:starts[k]], id3v1Tag(), junk, shine[starts[k]:])
		d, got, err := decodeMode(t, bytes.NewReader(data), lenient())
		if err != nil {
			t.Fatal(err)
		}
		r := d.Report()
		if !slices.Equal(got, want) || r.MidstreamTags != 1 || r.SkippedBytes != int64(len(junk)) || r.Resyncs != 1 ||
			r.TrailingTags != 0 || r.First == nil || *r.First != first {
			t.Fatalf("%d samples (want %d), report %+v, first %+v; want one mid-stream tag, %d bytes skipped, one resync, first %+v",
				len(got), len(want), r, r.First, len(junk), first)
		}
		checkGapOnlyAt(t, data, n, k)
	})
}

// 宽松模式丢弃了所有的帧,所以没有可以交付的音频,以 ErrNoAudio 结束。严格模式报截断。
func TestLenientNoAudio(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	data := shine[:200] // 第一帧只剩 200 字节
	if _, _, err := decodeMode(t, bytes.NewReader(data), Strict()); errClass(err) != "truncated" {
		t.Fatalf("strict: err=%v, want truncated", err)
	}
	if _, _, err := decodeMode(t, bytes.NewReader(data), lenient()); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("lenient: err=%v, want ErrNoAudio", err)
	}
}

// 只有标签帧、本来就没有音频帧的流(VBRI 声明 0 帧):两种模式都正常结束,没有样本,
// 报告干净。宽松模式的 ReadPCM 只在丢弃、跳过或截断了数据而一帧也没解出时返回
// ErrNoAudio。
func TestTagFrameOnlyStreamEndsCleanly(t *testing.T) {
	data := vbriStream(0, 0)
	for _, m := range []struct {
		name string
		opts []Option
	}{{"strict", []Option{Strict()}}, {"lenient", []Option{lenient()}}} {
		t.Run(m.name, func(t *testing.T) {
			d, pcm, err := decodeMode(t, bytes.NewReader(data), m.opts...)
			if d == nil {
				t.Fatal(err)
			}
			if r := d.Report(); err != nil || len(pcm) != 0 || !r.Clean() {
				t.Fatalf("err=%v, %d samples, report %+v; want a normal end without samples and a clean report", err, len(pcm), r)
			}
		})
	}
}

// 流里本来就没有音频帧(只有标签帧),异常至多是 emphasis 或标签超出预算:什么也没有丢,
// 宽松模式照常结束,没有样本,报告记下异常。ErrNoAudio 只用于丢了数据而一帧也没解出。
func TestTagFrameOnlyStreamWithAnomaliesEndsNormally(t *testing.T) {
	emphasis := vbriStream(0, 0)
	emphasis[3] |= 1
	for _, tc := range []struct {
		name  string
		src   func() io.Reader
		first Issue
	}{
		{"emphasis", func() io.Reader { return bytes.NewReader(emphasis) },
			Issue{Kind: IssueUnsupported, Frame: 0, Offset: 0}},
		{"after a tag over the 16 MiB budget", func() io.Reader {
			return io.MultiReader(bytes.NewReader(id3v2Header(17<<20)), io.LimitReader(zeroFill{}, 17<<20), bytes.NewReader(vbriStream(0, 0)))
		}, Issue{Kind: IssueLimit, Frame: 0, Offset: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, pcm, err := decodeMode(t, tc.src(), lenient())
			if d == nil {
				t.Fatal(err)
			}
			r := d.Report()
			if err != nil || len(pcm) != 0 || r.First == nil || *r.First != tc.first || r.AudioFrames != 0 {
				t.Fatalf("err=%v, %d samples, report %+v, first %+v; want a normal end without samples and first %+v", err, len(pcm), r, r.First, tc.first)
			}
		})
	}
}

// 一串标签中间有一个读不下去的:它之前读完的标签算流中间的标签(没有止于流尾),不能随这串
// 标签一起被忘掉。出问题的那个标签按损坏处理。
func TestBrokenTagKeepsTheTagsBeforeIt(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	n := len(starts)
	const k = 50
	_, want := decodeAll(t, bytes.NewReader(shine))
	ape := apeTag(true, [2]string{"Title", "gl3d"})
	for _, tc := range []struct {
		name      string
		data      []byte
		frames    int
		skipped   int64
		resyncs   int64
		truncated bool
		first     Issue
	}{
		{"APEv2 then a cut ID3v1 at the end", cat(shine, ape, id3v1Tag()[:60]), n, 60, 0, true,
			Issue{Kind: IssueTruncated, Frame: int64(n), Offset: int64(len(shine) + len(ape))}},
		{"ID3v1 then Lyrics3 without an end before frames", cat(shine[:starts[k]], id3v1Tag(), []byte("LYRICSBEGINla la"), shine[starts[k]:]), n, 16, 1, false,
			Issue{Kind: IssueCorrupt, Frame: k, Offset: int64(starts[k] + 128)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, got, err := decodeMode(t, bytes.NewReader(tc.data), lenient())
			if err != nil {
				t.Fatal(err)
			}
			r := d.Report()
			if !slices.Equal(got, want) || r.AudioFrames != int64(tc.frames) ||
				r.MidstreamTags != 1 || r.TrailingTags != 0 || r.SkippedBytes != tc.skipped || r.Resyncs != tc.resyncs ||
				r.Truncated != tc.truncated || r.First == nil || *r.First != tc.first {
				t.Fatalf("%d frames, report %+v (tags %q), first %+v; want %d frames, one mid-stream tag, %d bytes skipped, %d resyncs, truncated %v, first %+v",
					r.AudioFrames, r, r.TrailingTags, r.First, tc.frames, tc.skipped, tc.resyncs, tc.truncated, tc.first)
			}
		})
	}
}

// 只有 footer 的 APEv2 标签不为空:重新同步在垃圾里碰上它的 footer,认作标签起点,读下去
// 才发现条目数与长度对不上。失败时丢掉整个 APETAGEX 标识,而不是只丢一个字节让 "TAGEX"
// 被当作 ID3v1,吃掉其后的 128 字节。
func TestResyncSkipsAnAPEFooterWhole(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	n := len(starts)
	_, two := decodeAll(t, bytes.NewReader(cat(shine, shine)))
	_, one := decodeAll(t, bytes.NewReader(shine))
	junk := noSync(300)
	ape := apeTag(false, [2]string{"Title", "gl3d"}) // 条目加 footer,没有头部
	first := Issue{Kind: IssueCorrupt, Frame: int64(n - 1), Offset: int64(starts[n-1])}
	for _, tc := range []struct {
		name   string
		tail   []byte
		frames int
		pcm    []float64
	}{
		{"before frames", shine, 2 * n, two},
		{"at the end of the stream", nil, n, one},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, got, err := decodeMode(t, bytes.NewReader(cat(shine, junk, ape, tc.tail)), lenient())
			if err != nil {
				t.Fatal(err)
			}
			// 垃圾与整个 APEv2 都按垃圾跳过。停在 footer 的重新同步与其后找到帧的各算一次
			r := d.Report()
			wantResyncs := int64(1)
			if tc.tail != nil {
				wantResyncs = 2
			}
			if !slices.Equal(got, tc.pcm) || r.AudioFrames != int64(tc.frames) || r.SkippedBytes != int64(len(junk)+len(ape)) ||
				r.Resyncs != wantResyncs || r.TrailingTags != 0 || r.MidstreamTags != 0 || r.First == nil || *r.First != first {
				t.Fatalf("%d frames (want %d), report %+v, first %+v; want %d bytes skipped, %d resyncs, first %+v",
					r.AudioFrames, tc.frames, r, r.First, len(junk)+len(ape), wantResyncs, first)
			}
		})
	}
}

// 垃圾里的 APETAGEX 头无效(版本不对):重新同步整个跳过这个标识,而不是只跳一个字节让
// "TAGEX" 被当作 ID3v1,吃掉其后的 128 字节、连带下一份音频的第一帧。
func TestResyncSkipsAnInvalidAPEIdentifierWhole(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	n := len(starts)
	_, two := decodeAll(t, bytes.NewReader(cat(shine, shine)))
	junk := cat(noSync(100), []byte("APETAGEX"), make([]byte, 24)) // 版本 0
	d, got, err := decodeMode(t, bytes.NewReader(cat(shine, junk, shine)), lenient())
	if err != nil {
		t.Fatal(err)
	}
	r := d.Report()
	if !slices.Equal(got, two) || r.AudioFrames != int64(2*n) || r.SkippedBytes != int64(len(junk)) || r.Resyncs != 1 || r.MidstreamTags != 0 {
		t.Fatalf("%d frames (want %d), report %+v; want %d bytes skipped and one resync", r.AudioFrames, 2*n, r, len(junk))
	}
}

// 重新同步扫过长段垃圾时源出错或被取消:垃圾之前的帧照常交付,终止错误就是这次中断,报告
// 里第一处异常是垃圾,取消之后不再读取。
func TestLenientResyncInterrupted(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	const k = 5
	_, all := decodeAll(t, bytes.NewReader(shine))
	want := all[:k*1152*2]
	data := cat(shine[:starts[k]], noSync(300<<10))
	first := Issue{Kind: IssueCorrupt, Frame: k - 1, Offset: int64(starts[k-1])}

	t.Run("source error", func(t *testing.T) {
		sourceErr := errors.New("source reset")
		d, got, err := decodeMode(t, &dataWithErrReader{r: bytes.NewReader(data), err: sourceErr}, lenient())
		if d == nil {
			t.Fatal(err)
		}
		r := d.Report()
		if !errors.Is(err, sourceErr) || !slices.Equal(got, want) || r.SkippedBytes < 200<<10 || r.First == nil || *r.First != first {
			t.Fatalf("err=%v, %d samples (want %d), report %+v, first %+v; want the source error after the %d frames before the junk, first %+v",
				err, len(got), len(want), r, r.First, k, first)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		src := &cancelAfterReader{r: bytes.NewReader(data), limit: starts[k] + 100<<10, cancel: cancel}
		d, err := NewDecoder(ctx, src, lenient())
		if err != nil {
			t.Fatal(err)
		}
		var got []float64
		buf := make([]float64, 4096)
		for err == nil {
			var n int
			n, err = d.ReadPCM(ctx, buf)
			got = append(got, buf[:n*d.Channels()]...)
		}
		r := d.Report()
		if !errors.Is(err, context.Canceled) || !slices.Equal(got, want) || src.read >= len(data) || r.First == nil || *r.First != first {
			t.Fatalf("err=%v, %d samples (want %d), %d of %d bytes read, first %+v; want cancellation after the %d frames before the junk without reading it all, first %+v",
				err, len(got), len(want), src.read, len(data), r.First, k, first)
		}
	})
}
