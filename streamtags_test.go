package gl3d

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// apeTag 返回 APEv2 标签,条目为 {键, 文本值};header 为真时带头部。
func apeTag(header bool, items ...[2]string) []byte {
	var body []byte
	for _, it := range items {
		body = binary.LittleEndian.AppendUint32(body, uint32(len(it[1])))
		body = binary.LittleEndian.AppendUint32(body, 0)
		body = append(body, it[0]...)
		body = append(body, 0)
		body = append(body, it[1]...)
	}
	block := func(flags uint32) []byte {
		b := []byte("APETAGEX")
		b = binary.LittleEndian.AppendUint32(b, 2000)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(body)+apeTagSize))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(items)))
		b = binary.LittleEndian.AppendUint32(b, flags)
		return append(b, make([]byte, 8)...)
	}
	var out []byte
	var flags uint32
	if header {
		flags = 1 << 31 // 标签带头部
		out = block(flags | apeIsHeader)
	}
	out = append(out, body...)
	return append(out, block(flags)...)
}

func lyrics3v1(text string) []byte { return []byte("LYRICSBEGIN" + text + "LYRICSEND") }

// lyrics3v2 返回含 IND 与 LYR 两个字段的 Lyrics3 v2 标签。
func lyrics3v2(text string) []byte {
	b := []byte("LYRICSBEGIN")
	b = fmt.Appendf(b, "IND%05d%s", 2, "10")
	b = fmt.Appendf(b, "LYR%05d%s", len(text), text)
	return fmt.Appendf(b, "%06dLYRICS200", len(b))
}

func tagPlus() []byte {
	t := make([]byte, tagPlusSize)
	copy(t, "TAG+")
	return t
}

// 音频之后恰好是一串可识别的标签:两种模式都正常结束,输出与没有标签时逐字节
// 相同,标签种类记入报告,报告仍是干净的。
func TestTrailingTagChains(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	_, want := decodeAll(t, bytes.NewReader(shine))
	plusTitle := id3v1Tag()
	plusTitle[3] = '+' // 标题以 + 开头的 ID3v1 也以 "TAG+" 开头,其后没有 ID3v1 就不是 TAG+
	cases := []struct {
		name string
		tags []byte
		want TagSet
	}{
		{"APEv2 with header", apeTag(true, [2]string{"Title", "gl3d"}), TagAPEv2},
		{"APEv2 footer only", apeTag(false, [2]string{"Title", "gl3d"}, [2]string{"Artist", "x"}), TagAPEv2},
		{"APEv2 without items", apeTag(false), TagAPEv2},
		{"APEv2 key of 255 characters", apeTag(false, [2]string{strings.Repeat("k", 255), "v"}), TagAPEv2},
		{"APEv2 then ID3v1", cat(apeTag(true, [2]string{"Title", "gl3d"}), id3v1Tag()), TagAPEv2 | TagID3v1},
		{"Lyrics3 v2 then ID3v1", cat(lyrics3v2("[00:01]la"), id3v1Tag()), TagLyrics3 | TagID3v1},
		{"Lyrics3 v1 then ID3v1", cat(lyrics3v1("la la"), id3v1Tag()), TagLyrics3 | TagID3v1},
		{"TAG+ then ID3v1", cat(tagPlus(), id3v1Tag()), TagID3v1Ext | TagID3v1},
		{"ID3v1 titled +", plusTitle, TagID3v1},
		{"ID3v2 with footer", rawID3v2(4, 0x10, make([]byte, 40), true), TagID3v2},
		{"ID3v2 without footer", rawID3v2(3, 0, make([]byte, 40), false), TagID3v2},
		{"every kind", cat(apeTag(false, [2]string{"Title", "gl3d"}), lyrics3v2("la"), rawID3v2(4, 0x10, nil, true), tagPlus(), id3v1Tag()),
			TagAPEv2 | TagLyrics3 | TagID3v2 | TagID3v1Ext | TagID3v1},
		// 值长度 0xE0FF 的小端字节以 FF E0 开头,像帧同步;帧后与下一次调用必须对同一段字节作同样的判断
		{"APEv2 item length reads as a frame sync", apeTag(false, [2]string{"Cover", string(make([]byte, 0xE0FF))}), TagAPEv2},
		{"APEv2 item length reads as a frame sync, after ID3v1", cat(id3v1Tag(), apeTag(false, [2]string{"Cover", string(make([]byte, 0xE8FF))})), TagID3v1 | TagAPEv2},
		{"Lyrics3 v1 of 5100 bytes", cat(lyrics3v1(strings.Repeat("a", lyrics3v1Max)), id3v1Tag()), TagLyrics3 | TagID3v1},
		{"ID3v2 larger than the buffer", rawID3v2(3, 0, make([]byte, 200<<10), false), TagID3v2},
		{"APEv2 item larger than the buffer", apeTag(true, [2]string{"Cover", string(make([]byte, 150<<10))}), TagAPEv2},
	}
	modes := []struct {
		name string
		opts []Option
	}{{"strict", []Option{Strict()}}, {"lenient", []Option{lenient()}}}
	for _, tc := range cases {
		data := cat(shine, tc.tags)
		for _, m := range modes {
			for rname, mk := range map[string]func() io.Reader{
				"bytes":   func() io.Reader { return bytes.NewReader(data) },
				"onebyte": func() io.Reader { return &oneByteReader{r: bytes.NewReader(data)} },
			} {
				t.Run(tc.name+"/"+m.name+"/"+rname, func(t *testing.T) {
					d, got, err := decodeMode(t, mk(), m.opts...)
					if err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(got, want) {
						t.Fatalf("PCM differs from the untagged stream: %d samples, want %d", len(got), len(want))
					}
					if r := d.Report(); !r.Clean() || r.TrailingTags != tc.want {
						t.Fatalf("report %+v (tags %q), want clean with %q", r, r.TrailingTags, tc.want)
					}
				})
			}
		}
	}
}

// 严格模式下尾部标签与其长度字段或结束标记不符:已知范围的标签被流结束截断为
// 截断,其余为格式错误;报告记下无法确认的那个标签的起点。
func TestTrailingTagsMalformedStrict(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	frames, end := int64(len(starts)), int64(len(shine))
	ape := apeTag(true, [2]string{"Title", "gl3d"})
	badCount := apeTag(false, [2]string{"Title", "gl3d"})
	binary.LittleEndian.PutUint32(badCount[len(badCount)-apeTagSize+16:], 2) // footer 声明 2 个条目
	badSize := apeTag(false, [2]string{"Title", "gl3d"})
	binary.LittleEndian.PutUint32(badSize[len(badSize)-apeTagSize+12:], 99) // footer 声明的总长不符
	badLyrics := lyrics3v2("la")
	copy(badLyrics[len(badLyrics)-15:], "000001") // 6 位总长不符
	cases := []struct {
		name string
		tags []byte
		kind IssueKind
		at   int64 // 无法确认的标签的起点
		text string
	}{
		{"APEv2 cut inside items", ape[:40], IssueTruncated, end, "truncated APEv2 tag"},
		{"APEv2 footer item count", badCount, IssueCorrupt, end, "malformed APEv2 tag"},
		{"APEv2 footer size", badSize, IssueCorrupt, end, "malformed APEv2 tag"},
		{"Lyrics3 v2 size", cat(badLyrics, id3v1Tag()), IssueCorrupt, end, "malformed Lyrics3 tag"},
		{"Lyrics3 v1 without end marker", []byte("LYRICSBEGINla la"), IssueTruncated, end, "truncated Lyrics3 tag"},
		{"Lyrics3 v1 over 5100 bytes", lyrics3v1(strings.Repeat("a", lyrics3v1Max+1)), IssueCorrupt, end, "malformed Lyrics3 tag"},
		{"ID3v2 cut", id3v2Tag(100)[:50], IssueTruncated, end, "truncated ID3v2 tag"},
		{"junk after APEv2", cat(ape, []byte("junk")), IssueCorrupt, end, "unknown data after APEv2 tag"},
		{"junk after ID3v1 after APEv2", cat(ape, id3v1Tag(), []byte("junk")), IssueCorrupt, end + int64(len(ape)), "unknown data after ID3v1 tag"},
		{"TAG+ without ID3v1", tagPlus(), IssueCorrupt, end, "unknown data after ID3v1 tag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, err := decodeMode(t, bytes.NewReader(cat(shine, tc.tags)), Strict())
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err=%v, want one containing %q", err, tc.text)
			}
			if d.DecodedFrames() != frames*1152 {
				t.Fatalf("DecodedFrames()=%d, want %d: every audio frame is delivered", d.DecodedFrames(), frames*1152)
			}
			r := d.Report()
			if r.First == nil || *r.First != (Issue{Kind: tc.kind, Frame: frames, Offset: tc.at}) || r.TrailingTags != 0 {
				t.Fatalf("first %+v, tags %q, want %+v and no tags", r.First, r.TrailingTags, Issue{Kind: tc.kind, Frame: frames, Offset: tc.at})
			}
		})
	}
}

// 严格模式下标签之后又出现音频帧:这些标签在流中间,报格式错误并指向第一个标签;
// 标签之前的帧照常交出。
func TestMidstreamTagsStrict(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	frames := int64(len(starts))
	cases := []struct {
		name  string
		tags  []byte
		first string
	}{
		{"ID3v2", id3v2Tag(100), "ID3v2"},
		{"ID3v1", id3v1Tag(), "ID3v1"},
		{"APEv2 then ID3v1", cat(apeTag(true, [2]string{"Title", "gl3d"}), id3v1Tag()), "APEv2"},
		{"APEv2 whose item length reads as a frame sync", apeTag(false, [2]string{"Cover", string(make([]byte, 0xE0FF))}), "APEv2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, err := decodeMode(t, bytes.NewReader(cat(shine, tc.tags, shine)), Strict())
			var c *CorruptError
			if !errors.As(err, &c) || c.Detail != tc.first+" tag in mid-stream" || c.Frame != frames {
				t.Fatalf("err=%v, want CorruptError %q at frame %d", err, tc.first+" tag in mid-stream", frames)
			}
			if d.DecodedFrames() != frames*1152 {
				t.Fatalf("DecodedFrames()=%d, want %d", d.DecodedFrames(), frames*1152)
			}
			r := d.Report()
			if r.First == nil || *r.First != (Issue{Kind: IssueCorrupt, Frame: frames, Offset: int64(len(shine))}) || r.TrailingTags != 0 {
				t.Fatalf("report %+v, first %+v", r, r.First)
			}
		})
	}
}

// 尾部标签与开头的标签共用 16 MiB 的跳过预算;超出发生在交出音频之后,
// 同样记入报告。
func TestTrailingTagsShareBudget(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	lead := id3v2Tag(9 << 20)
	d, _, err := decodeMode(t, bytes.NewReader(cat(lead, shine, id3v2Tag(8<<20))), Strict())
	var l *LimitError
	if !errors.As(err, &l) {
		t.Fatalf("err=%v, want LimitError", err)
	}
	starts, _ := audioFrames(t, shine)
	want := Issue{Kind: IssueLimit, Frame: int64(len(starts)), Offset: int64(len(lead) + len(shine))}
	if r := d.Report(); r.Clean() || r.First == nil || *r.First != want {
		t.Fatalf("first %+v, want %+v", r.First, want)
	}
}

// 宽松模式不设累计的标签预算:超过 16 MiB 的标签照常跳过,只记一次资源限制。
func TestTagBudgetLenient(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	_, want := decodeAll(t, bytes.NewReader(shine))
	half := id3v2Tag(tagSkipBudget / 2)
	for _, tc := range []struct {
		name  string
		data  []byte
		first Issue
	}{
		{"leading", cat(id3v2Tag(tagSkipBudget), shine), Issue{Kind: IssueLimit, Frame: 0, Offset: 0}},
		{"two leading tags", cat(half, half, shine), Issue{Kind: IssueLimit, Frame: 0, Offset: int64(len(half))}},
		{"leading and trailing", cat(half, shine, half), Issue{Kind: IssueLimit, Frame: int64(len(starts)), Offset: int64(len(half) + len(shine))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, got, err := decodeMode(t, bytes.NewReader(tc.data), lenient())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatal("PCM differs from the untagged stream")
			}
			if r := d.Report(); r.Clean() || r.First == nil || *r.First != tc.first {
				t.Fatalf("report %+v, first %+v, want %+v", r, r.First, tc.first)
			}
		})
	}
}

// 帧后的数据只在恰好是标签起点时才确认帧边界:常见的填充与垃圾不会被认作标签。
func TestTagAtRejectsPaddingAndJunk(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"zeros", make([]byte, tagProbeSize)},
		{"text", []byte("not a frame, not a tag")},
		{"APETAGEX with version 0", append([]byte("APETAGEX"), make([]byte, 24)...)},
		{"ID3 with a size byte over 0x7F", []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0x80, 0}},
		// 头无效的 "ID3" 之后碰巧是一个 APE 条目(标志 0x80000000,键名 "ab")
		{"ID3 with an invalid header followed by an APE item", cat([]byte{'I', 'D', '3', 4, 0, 0, 0, 0x80, 'a', 'b', 0}, make([]byte, 20))},
		{"APE item header with a one-character key", append([]byte{1, 0, 0, 0, 0, 0, 0, 0, 'k', 0}, make([]byte, 20)...)},
		{"APE item header with reserved flag bits", append([]byte{1, 0, 0, 0, 8, 0, 0, 0}, "Title\x00x"...)},
	} {
		if kind := tagAt(tc.b); kind != 0 {
			t.Errorf("%s: tagAt=%q, want none", tc.name, kind)
		}
	}
}

// 音频之后跟着头无效的 "ID3",其后的字节碰巧像 APE 条目:它不是标签,严格模式在最后一帧
// 之后报同步丢失,而不是把后面的数据当 APEv2 条目一路跳过直到超出预算。
func TestInvalidID3AfterAudioIsNotAnAPEItem(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	junk := cat([]byte{'I', 'D', '3', 4, 0, 0, 0, 0x80, 'a', 'b', 0}, make([]byte, 100))
	_, _, err := decodeMode(t, bytes.NewReader(cat(shine, junk)), Strict())
	if errClass(err) != "corrupt" {
		t.Fatalf("err=%v, want a corrupt-stream error", err)
	}
}

// 源读取错误与取消不是流的结构:尾部标签链在任意一个字节处被中断时,已完整到达的
// 音频帧照常交付,终止错误是中断本身,报告什么也不记。
func TestInterruptedTrailingTagsDeliverFramesAndRecordNothing(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	// NewDecoder 读到首帧和下一帧的帧头才返回,取消落在其前它自己就失败了;音频至少两帧,
	// 中断才会落在尾部标签上
	audio := shine[:starts[2]]
	tail := cat(apeTag(false, [2]string{"Title", "gl3d"}), lyrics3v2("la"), rawID3v2(4, 0x10, nil, true), tagPlus(), id3v1Tag())
	full := cat(audio, tail)
	_, want := decodeAll(t, bytes.NewReader(audio))
	sourceErr := errors.New("source reset")
	modes := []struct {
		name string
		opts []Option
	}{{"strict", []Option{Strict()}}, {"lenient", []Option{lenient()}}}
	for _, m := range modes {
		for cut := 0; cut < len(tail); cut++ {
			d, got, err := decodeMode(t, &dataWithErrReader{r: bytes.NewReader(full[:len(audio)+cut]), err: sourceErr}, m.opts...)
			if !errors.Is(err, sourceErr) || d == nil || !slices.Equal(got, want) || d.Report().First != nil {
				t.Fatalf("%s, source error after %d tail bytes: err=%v, %d samples (want %d)", m.name, cut, err, len(got), len(want))
			}

			// 取消发生在读到第 len(audio)+cut 字节时;decodeMode 用不会取消的 context,这里自己循环
			ctx, cancel := context.WithCancel(context.Background())
			r := &cancelAfterReader{r: &oneByteReader{r: bytes.NewReader(full)}, limit: len(audio) + cut, cancel: cancel}
			d, err = NewDecoder(ctx, r, m.opts...)
			if err != nil {
				cancel()
				t.Fatalf("%s, cancel after %d tail bytes: NewDecoder: %v", m.name, cut, err)
			}
			var pcm []float64
			buf := make([]float64, 4096)
			for err == nil {
				var n int
				n, err = d.ReadPCM(ctx, buf)
				pcm = append(pcm, buf[:n*d.Channels()]...)
				if n == 0 && err == nil {
					cancel()
					t.Fatalf("%s, cancel after %d tail bytes: no progress: (0, nil)", m.name, cut)
				}
			}
			cancel()
			if !errors.Is(err, context.Canceled) || !slices.Equal(pcm, want) || d.Report().First != nil {
				t.Fatalf("%s, cancel after %d tail bytes: err=%v, %d samples (want %d)", m.name, cut, err, len(pcm), len(want))
			}
		}
	}
}
