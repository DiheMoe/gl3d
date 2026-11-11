package gl3d

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

// shineWithEmphasis 返回 shine 语料,其中第 [from, to) 帧的 emphasis 置为 1(50/15 µs),
// to 为负表示到最后一帧。该文件不带 CRC,改动帧头不影响校验。
func shineWithEmphasis(t *testing.T, from, to int) (data []byte, starts []int) {
	t.Helper()
	data = readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ = audioFrames(t, data)
	if to < 0 {
		to = len(starts)
	}
	for _, s := range starts[from:to] {
		if data[s+1]&1 == 0 {
			t.Fatal("fixture: frame carries a CRC")
		}
		data[s+3] = data[s+3]&^3 | 1
	}
	return data, starts
}

// emphasis 非零时严格模式报不支持。宽松模式照常解码(不做去加重,与 FFmpeg 相同),
// 输出与 emphasis 为零的流逐字节相同,首帧的值与第一处异常记入报告。
func TestEmphasis(t *testing.T) {
	_, want := decodeAll(t, bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")))
	for _, tc := range []struct {
		name     string
		from, to int // 置 emphasis 的帧区间 [from, to),to 为负表示到最后一帧
		emphasis int // 报告中首帧的值
	}{
		{"every frame", 0, -1, 1},
		// 首帧本身干净,由其后的 emphasis 帧确认,所以严格模式解出首帧,到第 1 帧才报不支持
		{"from frame 1", 1, -1, 0},
		{"from frame 100", 100, -1, 0},
		// 两种模式选同一个首帧。只有首帧带 emphasis 时,严格模式也报不支持,而不是把它当垃圾跳过
		{"first frame only", 0, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, starts := shineWithEmphasis(t, tc.from, tc.to)
			first := Issue{Kind: IssueUnsupported, Frame: int64(tc.from), Offset: int64(starts[tc.from])}

			d, _, err := decodeMode(t, bytes.NewReader(data), Strict())
			var u *UnsupportedError
			if !errors.As(err, &u) || u.Feature != "non-zero emphasis" {
				t.Fatalf("strict: err=%v, want UnsupportedError %q", err, "non-zero emphasis")
			}
			// 首帧带 emphasis 时 NewDecoder 失败,没有报告。
			// 否则首帧照常解出,到带 emphasis 的帧才报错
			if (d == nil) != (tc.from == 0) {
				t.Fatalf("strict: decoder built = %v, want %v", d != nil, tc.from != 0)
			}
			if d != nil {
				if r := d.Report(); r.First == nil || *r.First != first {
					t.Fatalf("strict: first %+v, want %+v", r.First, first)
				}
			}

			d, got, err := decodeMode(t, bytes.NewReader(data), lenient())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatal("lenient: PCM differs from the stream without emphasis")
			}
			if r := d.Report(); r.Clean() || r.Emphasis != tc.emphasis || r.First == nil || *r.First != first {
				t.Fatalf("lenient: report %+v, first %+v; want emphasis %d, first %+v", r, r.First, tc.emphasis, first)
			}
		})
	}
}

// 严格模式在 open(首帧)与 next(其后的帧)报 emphasis 时都返回新的错误值,所以调用方
// 改动拿到的错误,不影响别的解码器。
func TestEmphasisErrorNotShared(t *testing.T) {
	for _, from := range []int{0, 1} {
		data, _ := shineWithEmphasis(t, from, -1)
		var errs [2]*UnsupportedError
		for i := range errs {
			_, _, err := decodeMode(t, bytes.NewReader(data), Strict())
			if !errors.As(err, &errs[i]) {
				t.Fatalf("from frame %d: err=%v, want UnsupportedError", from, err)
			}
		}
		errs[0].Feature = "changed by the caller"
		if errs[0] == errs[1] || errs[1].Feature != "non-zero emphasis" {
			t.Fatalf("from frame %d: strict decoders share one error value", from)
		}
	}
}

// 首帧是 Info 标签帧时,open 把它整帧跳过,next 不会再看到它,所以 emphasis 须在 open 里先于
// 跳过检查。改动帧头使 LAME 标签的 CRC 失效,gapless 字段随之弃用,因此不与原文件比较 PCM。
func TestEmphasisOnTagFrame(t *testing.T) {
	data := readCorpus(t, "tone10ms-stereo-44k1-cbr128-lame.mp3")
	starts, _ := audioFrames(t, data)
	h, err := parseFrameHeader([4]byte(data[:4]))
	if err != nil {
		t.Fatal(err)
	}
	if magic, _ := metaMagic(data[:h.frameSize], h); magic == "" {
		t.Fatal("fixture: first frame is not a tag frame")
	}
	data[3] = data[3]&^3 | 1

	d, _, err := decodeMode(t, bytes.NewReader(data), Strict())
	var u *UnsupportedError
	if d != nil || !errors.As(err, &u) || u.Feature != "non-zero emphasis" {
		t.Fatalf("strict: decoder built = %t, err=%v; want no decoder and UnsupportedError %q", d != nil, err, "non-zero emphasis")
	}

	d, _, err = decodeMode(t, bytes.NewReader(data), lenient())
	if err != nil {
		t.Fatal(err)
	}
	first := Issue{Kind: IssueUnsupported, Frame: 0, Offset: 0}
	if r := d.Report(); r.Clean() || r.Emphasis != 1 || r.First == nil || *r.First != first || r.AudioFrames != int64(len(starts)) {
		t.Fatalf("lenient: report %+v, first %+v; want emphasis 1, first %+v, %d audio frames", r, r.First, first, len(starts))
	}
}
