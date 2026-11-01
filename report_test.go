package gl3d

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func drainReport(t *testing.T, d *Decoder) (Report, error) {
	t.Helper()
	buf := make([]float64, 4096*d.Channels())
	for {
		if _, err := d.ReadPCM(context.Background(), buf); err != nil {
			return d.Report(), err
		}
	}
}

// 完整的流:帧数与 Info 声明一致,没有异常。
func TestReportCleanStream(t *testing.T) {
	d, err := NewDecoder(context.Background(), bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")), Strict())
	if err != nil {
		t.Fatal(err)
	}
	if r := d.Report(); r.AudioFrames != 0 || r.DeclaredFrames != 193 || !r.Clean() {
		t.Fatalf("before decoding: %+v", r)
	}
	r, err := drainReport(t, d)
	if err != io.EOF {
		t.Fatal(err)
	}
	if !r.Clean() || r.First != nil || r.AudioFrames != 193 || r.DeclaredFrames != 193 || r.TrailingTags != 0 {
		t.Fatalf("report %+v", r)
	}
}

// 没有标签帧:没有帧数声明;末尾的 ID3v1 是合法结构,不算异常。
func TestReportTrailingID3v1WithoutDeclaration(t *testing.T) {
	data := append(readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3"), id3v1Tag()...)
	d, err := NewDecoder(context.Background(), bytes.NewReader(data), Strict())
	if err != nil {
		t.Fatal(err)
	}
	r, err := drainReport(t, d)
	if err != io.EOF {
		t.Fatal(err)
	}
	if !r.Clean() || r.DeclaredFrames != -1 || r.AudioFrames != 191 || r.TrailingTags != TagID3v1 {
		t.Fatalf("report %+v", r)
	}
}

// 以格式错误终止时,第一处异常记下错误的类别、帧号与该帧的偏移。
func TestReportRecordsTerminalFormatError(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	t.Run("lost sync", func(t *testing.T) {
		data := append(bytes.Clone(shine), make([]byte, 16)...)
		d, err := NewDecoder(context.Background(), bytes.NewReader(data), Strict())
		if err != nil {
			t.Fatal(err)
		}
		r, err := drainReport(t, d)
		var c *CorruptError
		if !errors.As(err, &c) {
			t.Fatalf("err=%v, want CorruptError", err)
		}
		if r.Clean() || r.First == nil || r.First.Kind != IssueCorrupt || r.First.Frame != c.Frame || r.First.Frame != 190 {
			t.Fatalf("report %+v, first %+v", r, r.First)
		}
		// 第 190 帧是原文件的最后一帧,它后面的同步丢失在交出它之前就被发现:
		// 这一帧被扣下、没有解码,不计入已解码的帧数
		if r.AudioFrames != 190 {
			t.Fatalf("AudioFrames=%d, want 190 (the withheld frame is not counted)", r.AudioFrames)
		}
		// 偏移指向出错帧的帧头,该帧正好延伸到原文件末尾
		off := r.First.Offset
		if off <= 0 || off >= int64(len(shine)) || shine[off] != 0xFF {
			t.Fatalf("offset %d", off)
		}
		fh, err := parseFrameHeader([4]byte(shine[off : off+4]))
		if err != nil || off+int64(fh.frameSize) != int64(len(shine)) {
			t.Fatalf("offset %d: header %+v, %v", off, fh, err)
		}
	})
	t.Run("truncated frame body", func(t *testing.T) {
		data := shine[:len(shine)-100]
		d, err := NewDecoder(context.Background(), bytes.NewReader(data), Strict())
		if err != nil {
			t.Fatal(err)
		}
		r, err := drainReport(t, d)
		var tr *TruncatedError
		if !errors.As(err, &tr) {
			t.Fatalf("err=%v, want TruncatedError", err)
		}
		if r.Clean() || r.First == nil || r.First.Kind != IssueTruncated || r.First.Offset != tr.Offset || r.First.Frame != r.AudioFrames {
			t.Fatalf("report %+v, first %+v", r, r.First)
		}
	})
}

// 每一种终止解码的格式错误都要在报告里留下类别、帧号与偏移。偏移取自帧头解析
// 与 len,不手算;各例还断言终止错误的类型与文本,保证输入确实走到了预期的分支,
// 而不是碰巧得到同样的位置。
func TestReportPinsTerminalIssues(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	shineStarts, shineEnd := audioFrames(t, shine)
	if len(shineStarts) != 191 || shineEnd != len(shine) {
		t.Fatalf("fixture: %d frames ending at %d of %d bytes, want 191 frames ending at EOF", len(shineStarts), shineEnd, len(shine))
	}
	shineFrames, shineLen := int64(len(shineStarts)), int64(len(shine))

	info := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3") // Info 声明 193 帧
	infoStarts, _ := audioFrames(t, info)
	if len(infoStarts) != 193 {
		t.Fatalf("fixture: %d audio frames, want 193", len(infoStarts))
	}

	// 构造同 TestFrameCRCMismatch:翻转第 5 帧 side information 的一位,它在 CRC 覆盖范围内
	crc := readCorpus(t, "pluck-stereo-32k-cbr96-crc-lame.mp3")
	crcStarts, _ := audioFrames(t, crc)
	crc[crcStarts[5]+6+6] ^= 0x01

	vbri := vbriStream(6, 5) // 声明 6 帧,实有 5 帧

	// 构造同 TestGaplessPaddingExceedsDecoded:声明的尾部填充超过可裁剪的输出
	padded := readCorpus(t, "short-tone-stereo-44k1-cbr192.mp3") // Info 声明 3 帧
	pp, _, plo := infoFrame(t, padded)
	padded[pp+plo+22] |= 0x0F // enc_padding = 0xFFF
	padded[pp+plo+23] = 0xFF
	setLAMETagCRC(padded[pp:], plo)

	cases := []struct {
		name    string
		data    []byte
		want    Report // First 必须非 nil;TrailingTags 不写即为 0:这些例子都没有取得完整的 ID3v1 标签
		errText string // 终止错误的文本须包含它
	}{
		{
			// 分帧层此时已交出 6 帧(含出错的第 5 帧),解码器只解码了 5 帧:AudioFrames 取后者
			name: "CRC mismatch at frame 5",
			data: crc,
			want: Report{
				DeclaredFrames: int64(len(crcStarts)),
				AudioFrames:    5,
				First:          &Issue{Kind: IssueCorrupt, Frame: 5, Offset: int64(crcStarts[5])},
			},
			errText: "CRC",
		},
		{
			name: "Layer II continuation",
			data: append(bytes.Clone(shine), repeatFrames(mkHeader(3, 2, 1, 9, 0, 0, 0, 0, 0), 417, 2)...),
			want: Report{
				DeclaredFrames: -1,
				AudioFrames:    shineFrames,
				First:          &Issue{Kind: IssueUnsupported, Frame: shineFrames, Offset: shineLen},
			},
			errText: "Layer II",
		},
		{
			name: "48 kHz frames",
			data: append(bytes.Clone(shine), repeatFrames(mkHeader(3, 1, 1, 11, 1, 0, 0, 0, 0), 576, 2)...), // 192 kbps @ 48 kHz:576 B
			want: Report{
				DeclaredFrames: -1,
				AudioFrames:    shineFrames,
				First:          &Issue{Kind: IssueUnsupported, Frame: shineFrames, Offset: shineLen},
			},
			errText: "mid-stream format change",
		},
		{
			name: "VBRI declares one frame too many",
			data: vbri,
			want: Report{
				DeclaredFrames: 6,
				AudioFrames:    5,
				First:          &Issue{Kind: IssueTruncated, Frame: 5, Offset: int64(len(vbri))},
			},
			errText: "VBRI declaration",
		},
		{
			name: "Xing declares one frame too many",
			data: info[:infoStarts[192]], // 在最后一帧的起点截断:流在帧边界结束,但帧数少于声明
			want: Report{
				DeclaredFrames: 193,
				AudioFrames:    192,
				First:          &Issue{Kind: IssueTruncated, Frame: 192, Offset: int64(infoStarts[192])},
			},
			errText: "Xing declaration",
		},
		{
			name: "ID3v1 tag cut to 100 bytes",
			data: append(bytes.Clone(shine), id3v1Tag()[:100]...),
			want: Report{
				DeclaredFrames: -1,
				AudioFrames:    shineFrames,
				First:          &Issue{Kind: IssueTruncated, Frame: shineFrames, Offset: shineLen},
			},
			errText: "ID3v1 tag",
		},
		{
			name: "ID3v1 tag followed by 3 bytes",
			data: append(append(bytes.Clone(shine), id3v1Tag()...), 0, 0, 0),
			want: Report{
				DeclaredFrames: -1,
				AudioFrames:    shineFrames,
				First:          &Issue{Kind: IssueCorrupt, Frame: shineFrames, Offset: shineLen},
			},
			errText: "unknown data after ID3v1 tag",
		},
		{
			// 帧数与声明一致、流正常结束,矛盾在流结束时才报告,无法定位到帧
			name: "gapless padding exceeds decoded length",
			data: padded,
			want: Report{
				DeclaredFrames: 3,
				AudioFrames:    3,
				First:          &Issue{Kind: IssueCorrupt, Frame: -1, Offset: int64(len(padded))},
			},
			errText: "gapless padding",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDecoder(context.Background(), bytes.NewReader(tc.data), Strict())
			if err != nil {
				t.Fatal(err)
			}
			r, err := drainReport(t, d)
			if err == io.EOF {
				t.Fatalf("stream ended cleanly, want a %v error", tc.want.First.Kind)
			}
			var typeOK bool
			switch tc.want.First.Kind {
			case IssueTruncated:
				var e *TruncatedError
				typeOK = errors.As(err, &e)
			case IssueCorrupt:
				var e *CorruptError
				typeOK = errors.As(err, &e)
			case IssueUnsupported:
				var e *UnsupportedError
				typeOK = errors.As(err, &e)
			}
			if !typeOK || !strings.Contains(err.Error(), tc.errText) {
				t.Fatalf("err=%v (%T), want a %v error containing %q", err, err, tc.want.First.Kind, tc.errText)
			}
			if r.Clean() {
				t.Fatalf("Clean() is true after %v", err)
			}
			if r.First == nil || *r.First != *tc.want.First {
				t.Fatalf("first %+v, want %+v (err %v)", r.First, tc.want.First, err)
			}
			if r.AudioFrames != tc.want.AudioFrames || r.DeclaredFrames != tc.want.DeclaredFrames || r.TrailingTags != tc.want.TrailingTags {
				t.Fatalf("AudioFrames=%d DeclaredFrames=%d TrailingTags=%q, want %d %d %q (err %v)",
					r.AudioFrames, r.DeclaredFrames, r.TrailingTags,
					tc.want.AudioFrames, tc.want.DeclaredFrames, tc.want.TrailingTags, err)
			}
		})
	}
}

// 源读取错误不是流的结构问题,不记为异常。
func TestReportIgnoresSourceErrors(t *testing.T) {
	sourceErr := errors.New("source reset")
	d, err := NewDecoder(context.Background(), &dataWithErrReader{r: bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")), err: sourceErr}, Strict())
	if err != nil {
		t.Fatal(err)
	}
	r, err := drainReport(t, d)
	if !errors.Is(err, sourceErr) {
		t.Fatalf("err=%v", err)
	}
	if !r.Clean() || r.First != nil {
		t.Fatalf("report %+v", r)
	}
}

// 异常类别的判断:直接的错误值与包装过的错误结果相同,源错误不属于任何类别;判断本身
// 与识别 ID3v2 头一样不分配内存(每个垃圾字节都可能走到这里)。
func TestIssueKindClassifiesWithoutAllocating(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want IssueKind
	}{
		{&TruncatedError{}, IssueTruncated},
		{&CorruptError{}, IssueCorrupt},
		{&UnsupportedError{}, IssueUnsupported},
		{&LimitError{}, IssueLimit},
		{fmt.Errorf("wrapped: %w", &TruncatedError{}), IssueTruncated},
		{fmt.Errorf("wrapped: %w", &LimitError{}), IssueLimit},
		{errors.New("source reset"), 0},
		{ErrNoAudio, 0},
	} {
		if got := issueKind(tc.err); got != tc.want {
			t.Errorf("issueKind(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
	for _, err := range []error{&TruncatedError{}, &CorruptError{}, &UnsupportedError{}, &LimitError{}} {
		if n := testing.AllocsPerRun(100, func() { issueKind(err) }); n != 0 {
			t.Errorf("issueKind(%T) allocates %v times per call", err, n)
		}
	}
	badID3 := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0x80, 0}
	if n := testing.AllocsPerRun(100, func() { magicTagAt(badID3) }); n != 0 {
		t.Errorf("magicTagAt on an ID3 header with a bad size allocates %v times per call", n)
	}
}

// Report 返回的 First 是副本,调用方改动它不影响解码器。
func TestReportFirstIsACopy(t *testing.T) {
	data := append(readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3"), make([]byte, 16)...)
	d, err := NewDecoder(context.Background(), bytes.NewReader(data), Strict())
	if err != nil {
		t.Fatal(err)
	}
	r, _ := drainReport(t, d)
	r.First.Frame = -7
	if d.Report().First.Frame == -7 {
		t.Fatal("Report exposes the decoder's issue")
	}
}

func TestReportLogValue(t *testing.T) {
	var out bytes.Buffer
	r := Report{DeclaredFrames: 193, AudioFrames: 190, DroppedFrames: 3, ConcealedGranules: 5, CRCErrors: 2, SkippedBytes: 4096, Resyncs: 2, MidstreamTags: 2, Truncated: true, Emphasis: 1, TrailingTags: TagID3v1, First: &Issue{Kind: IssueCorrupt, Frame: 190, Offset: 1234}}
	slog.New(slog.NewJSONHandler(&out, nil)).Info("decoded", "gl3d", r)
	var got struct {
		GL3D struct {
			Clean             bool   `json:"clean"`
			AudioFrames       int64  `json:"audio_frames"`
			DeclaredFrames    int64  `json:"declared_frames"`
			DroppedFrames     int64  `json:"dropped_frames"`
			ConcealedGranules int64  `json:"concealed_granules"`
			CRCErrors         int64  `json:"crc_errors"`
			SkippedBytes      int64  `json:"skipped_bytes"`
			Resyncs           int64  `json:"resyncs"`
			MidstreamTags     int64  `json:"midstream_tags"`
			Truncated         bool   `json:"truncated"`
			Emphasis          int    `json:"emphasis"`
			TrailingTags      string `json:"trailing_tags"`
			First             struct {
				Kind   string `json:"kind"`
				Frame  int64  `json:"frame"`
				Offset int64  `json:"offset"`
			} `json:"first"`
		} `json:"gl3d"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err, out.String())
	}
	g := got.GL3D
	if g.Clean || g.AudioFrames != 190 || g.DeclaredFrames != 193 || g.DroppedFrames != 3 || g.ConcealedGranules != 5 || g.CRCErrors != 2 || g.SkippedBytes != 4096 || g.Resyncs != 2 || !g.Truncated ||
		g.MidstreamTags != 2 || g.Emphasis != 1 || g.TrailingTags != "ID3v1" ||
		g.First.Kind != "corrupt" || g.First.Frame != 190 || g.First.Offset != 1234 {
		t.Fatalf("log %s", out.String())
	}
	// 没有异常、流中间的标签、尾部标签与 emphasis 时不输出这些字段
	out.Reset()
	slog.New(slog.NewJSONHandler(&out, nil)).Info("decoded", "gl3d", Report{DeclaredFrames: -1, AudioFrames: 3})
	for _, key := range []string{"first", "trailing_tags", "midstream_tags", "emphasis", "dropped_frames", "concealed_granules", "crc_errors", "skipped_bytes", "resyncs", "truncated"} {
		if bytes.Contains(out.Bytes(), []byte(`"`+key+`"`)) {
			t.Fatalf("log %s: unexpected %q", out.String(), key)
		}
	}
}

// 类别与标签集合的文本会进入日志,逐值固定;零值的类别是 unknown,空集合是空串。
func TestReportStrings(t *testing.T) {
	for _, tc := range []struct {
		kind IssueKind
		want string
	}{
		{IssueTruncated, "truncated"},
		{IssueCorrupt, "corrupt"},
		{IssueUnsupported, "unsupported"},
		{IssueLimit, "limit"},
		{IssueKind(0), "unknown"},
	} {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("IssueKind(%d).String() = %q, want %q", int(tc.kind), got, tc.want)
		}
	}
	for _, tc := range []struct {
		tags TagSet
		want string
	}{
		{0, ""},
		{TagID3v1, "ID3v1"},
		{TagID3v1 | TagAPEv2, "ID3v1|APEv2"},
		{TagID3v1 | TagID3v1Ext | TagAPEv2 | TagLyrics3 | TagID3v2, "ID3v1|TAG+|APEv2|Lyrics3|ID3v2"},
	} {
		if got := tc.tags.String(); got != tc.want {
			t.Errorf("TagSet(%d).String() = %q, want %q", uint8(tc.tags), got, tc.want)
		}
	}
}
