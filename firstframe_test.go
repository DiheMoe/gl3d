package gl3d

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

// countingReader 记录读出的字节数。
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// NewDecoder 只读取确认首帧所需的数据:帧本身加下一帧的 4 字节帧头;范围外格式同样
// 在帧长处确认,不等满 4 KiB 的窗口。逐字节的源上读出的字节数就是请求的字节数。
func TestNewDecoderReadsOnlyWhatItNeeds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  []byte
		want  int
		fails bool
	}{
		{"layer III", repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 20), 417 + 4, false},
		{"layer II", repeatFrames(mkHeader(3, 2, 1, 10, 0, 0, 0, 0, 0), 626, 20), 626 + 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &countingReader{r: &oneByteReader{r: bytes.NewReader(tc.data)}}
			_, err := NewDecoder(context.Background(), c, Strict())
			if (err != nil) != tc.fails {
				t.Fatalf("err=%v", err)
			}
			if c.n != tc.want {
				t.Fatalf("read %d bytes, want %d", c.n, tc.want)
			}
		})
	}
}

// 只有一帧、其后是标签的流:两种模式都确认这一帧。单独的 ID3v1 只有 3 字节标识,
// 要求标签之后即流结束;其余标签有长度字段或多字节标识,看到起点即可。
func TestFirstFrameFollowedByTags(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)
	one := shine[:starts[1]]
	for _, tc := range []struct {
		name string
		tags []byte
		want TagSet
	}{
		{"ID3v1", id3v1Tag(), TagID3v1},
		{"APEv2 with header", apeTag(true, [2]string{"Title", "gl3d"}), TagAPEv2},
		{"APEv2 footer only then ID3v1", cat(apeTag(false, [2]string{"Title", "gl3d"}), id3v1Tag()), TagAPEv2 | TagID3v1},
		{"Lyrics3 v2 then ID3v1", cat(lyrics3v2("la"), id3v1Tag()), TagLyrics3 | TagID3v1},
		{"ID3v2", rawID3v2(4, 0x10, make([]byte, 20), true), TagID3v2},
	} {
		for _, m := range []struct {
			name string
			opts []Option
		}{{"strict", []Option{Strict()}}, {"lenient", []Option{lenient()}}} {
			t.Run(tc.name+"/"+m.name, func(t *testing.T) {
				d, _, err := decodeMode(t, bytes.NewReader(cat(one, tc.tags)), m.opts...)
				if err != nil {
					t.Fatal(err)
				}
				if d.DecodedFrames() != 1152 {
					t.Fatalf("DecodedFrames()=%d, want 1152", d.DecodedFrames())
				}
				if r := d.Report(); !r.Clean() || r.TrailingTags != tc.want {
					t.Fatalf("report %+v (tags %q), want clean with %q", r, r.TrailingTags, tc.want)
				}
			})
		}
	}
}

// 源在首帧完整到达之后、下一帧帧头之前出错:首帧按未确认接受,先解码交付,再报告
// 源错误;未确认的首帧解码失败时(多半是伪同步)报告源错误,而不是格式错误。
func TestFirstFrameBeforeSourceError(t *testing.T) {
	sourceErr := errors.New("source reset")
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	starts, _ := audioFrames(t, shine)

	t.Run("frame decodes", func(t *testing.T) {
		r := &dataWithErrReader{r: bytes.NewReader(shine[:starts[1]+2]), err: sourceErr}
		d, pcm, err := decodeMode(t, r, Strict())
		if !errors.Is(err, sourceErr) {
			t.Fatalf("err=%v, want the source error", err)
		}
		if len(pcm) != 1152*2 {
			t.Fatalf("%d samples before the error, want the first frame (%d)", len(pcm), 1152*2)
		}
		if r := d.Report(); !r.Clean() {
			t.Fatalf("report %+v: a source error is not a property of the stream", r)
		}
	})

	// 宽松模式也不容错未确认的首帧:它多半是伪同步,丢弃它再报告成功会掩盖源错误
	for _, m := range bothModes {
		t.Run("frame fails to decode/"+m.name, func(t *testing.T) {
			frame := bytes.Clone(shine[:starts[1]])
			setBits(frame[4:], 32, 9, 511) // gr0/ch0 的 big_values 超过 288
			r := &dataWithErrReader{r: bytes.NewReader(append(frame, 0xFF)), err: sourceErr}
			d, pcm, err := decodeMode(t, r, m.opts...)
			if !errors.Is(err, sourceErr) {
				t.Fatalf("err=%v, want the source error", err)
			}
			if len(pcm) != 0 {
				t.Fatalf("%d samples delivered from a frame that fails to decode", len(pcm))
			}
			if rep := d.Report(); !rep.Clean() || rep.DroppedFrames != 0 {
				t.Fatalf("report %+v: an unconfirmed frame must fail, not be concealed", rep)
			}
		})
	}
}

// 帧长处既不是帧头也不是标签的候选是伪同步,不确认;单独的 ID3v1 要求标签完整且其后
// 即流结束,有 TAG+ 标识的扩展标签看到起点即可。逐字节的源上结果相同。
func TestFirstFrameConfirmationByWhatFollows(t *testing.T) {
	frame := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 1)
	for _, tc := range []struct {
		name string
		tail []byte
		ok   bool
	}{
		{"ID3v1 then end", id3v1Tag(), true},
		{"TAG+ then ID3v1", cat(tagPlus(), id3v1Tag()), true},
		{"ID3v1 then more data", cat(id3v1Tag(), []byte{0}), false},
		{"ID3v1 cut short", id3v1Tag()[:127], false},
		{"neither frame nor tag", make([]byte, 16), false},
	} {
		for _, m := range []struct {
			name string
			opts []Option
		}{{"strict", []Option{Strict()}}, {"lenient", []Option{lenient()}}} {
			t.Run(tc.name+"/"+m.name, func(t *testing.T) {
				r := &oneByteReader{r: bytes.NewReader(cat(frame, tc.tail))}
				_, err := NewDecoder(context.Background(), r, m.opts...)
				if tc.ok && err != nil {
					t.Fatalf("err=%v, want the first frame confirmed", err)
				}
				if !tc.ok && !errors.Is(err, ErrNoAudio) {
					t.Fatalf("err=%v, want ErrNoAudio: the candidate is not confirmed", err)
				}
			})
		}
	}
}

// 范围外格式的首帧在帧长处确认:流恰在帧后结束也算;帧不完整或多出数据则不算。
func TestOutOfRangeFirstFrameAtStreamEnd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		frame   []byte
		feature string
	}{
		{"layer II", repeatFrames(mkHeader(3, 2, 1, 10, 0, 0, 0, 0, 0), 626, 1), "MPEG Layer II"},
		{"layer I", repeatFrames(mkHeader(3, 3, 1, 8, 0, 0, 0, 0, 0), 276, 1), "MPEG Layer I"},
	} {
		t.Run(tc.name+"/frame then end", func(t *testing.T) {
			_, err := NewDecoder(context.Background(), bytes.NewReader(tc.frame), Strict())
			var u *UnsupportedError
			if !errors.As(err, &u) || u.Feature != tc.feature {
				t.Fatalf("err=%v, want unsupported %q", err, tc.feature)
			}
		})
		for name, data := range map[string][]byte{
			"one byte short": tc.frame[:len(tc.frame)-1],
			"one byte more":  append(bytes.Clone(tc.frame), 0),
		} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				_, err := NewDecoder(context.Background(), bytes.NewReader(data), Strict())
				if !errors.Is(err, ErrNoAudio) {
					t.Fatalf("err=%v, want ErrNoAudio: the candidate is not confirmed", err)
				}
			})
		}
	}
}

// 只有完整到达的候选才在源出错时按未确认接受;取消不是源错误,原样返回。
func TestFirstFrameAcceptedUnconfirmedOnlyWhenComplete(t *testing.T) {
	data := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 3)

	t.Run("incomplete at source error", func(t *testing.T) {
		sourceErr := errors.New("source reset")
		r := &dataWithErrReader{r: bytes.NewReader(data[:416]), err: sourceErr}
		d, err := NewDecoder(context.Background(), r, Strict())
		if d != nil || !errors.Is(err, sourceErr) {
			t.Fatalf("decoder=%t err=%v, want no decoder and the source error", d != nil, err)
		}
	})

	// 不完整的候选即使被接受,open 读取首帧时也会以同一个源错误失败,上面的例子分不出
	// 两者:这里直接核对分帧层只接受完整到达的候选
	t.Run("framer accepts only a complete candidate", func(t *testing.T) {
		sourceErr := errors.New("source reset")
		for _, tc := range []struct {
			have int
			ok   bool
		}{{416, false}, {417, true}} {
			fr := newFramer(&dataWithErrReader{r: bytes.NewReader(data[:tc.have]), err: sourceErr}, true)
			fillErr := fr.s.fill(context.Background(), 417+4)
			ok, err := fr.unconfirmedFirst(fillErr, frameHeader{frameSize: 417})
			if ok != tc.ok || fr.unconfirmed != tc.ok || !ok && !errors.Is(err, sourceErr) {
				t.Fatalf("%d bytes: ok=%v unconfirmed=%v err=%v, want ok=%v", tc.have, ok, fr.unconfirmed, err, tc.ok)
			}
		}
	})

	t.Run("canceled after the frame arrived", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := &cancelAfterReader{r: &oneByteReader{r: bytes.NewReader(data)}, limit: 417 + 2, cancel: cancel}
		d, err := NewDecoder(ctx, r, Strict())
		if d != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("decoder=%t err=%v, want no decoder and context.Canceled", d != nil, err)
		}
	})
}

// bothModes 是首帧确认须同样成立的两种模式。
var bothModes = []struct {
	name string
	opts []Option
}{{"strict", []Option{Strict()}}, {"lenient", []Option{lenient()}}}

// sliceErr 的动态类型不可比较:对两个这样的错误做 == 比较会 panic。
type sliceErr []string

func (sliceErr) Error() string { return "source reset" }

// 源错误的动态类型不可比较时,分帧与解码不得 panic,错误原样交给调用方。
func TestUncomparableSourceError(t *testing.T) {
	want := sliceErr{"source reset"}
	isWant := func(err error) bool {
		var got sliceErr
		return errors.As(err, &got) && slices.Equal(got, want)
	}
	frames := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 2)

	// 完整到达的首帧按未确认接受,先交付,再报告源错误,与可比较的错误相同
	t.Run("complete first frame", func(t *testing.T) {
		r := &dataWithErrReader{r: bytes.NewReader(frames[:417]), err: want}
		d, pcm, err := decodeMode(t, r, Strict())
		if d == nil || len(pcm) != 1152*2 || !isWant(err) {
			t.Fatalf("decoder=%t, %d samples, err=%v; want the first frame (%d samples) and then the source error", d != nil, len(pcm), err, 1152*2)
		}
	})

	t.Run("error at every byte", func(t *testing.T) {
		for _, m := range bothModes {
			for cut := 0; cut <= len(frames); cut++ {
				r := &dataWithErrReader{r: bytes.NewReader(frames[:cut]), err: want}
				if _, _, err := decodeMode(t, r, m.opts...); !isWant(err) {
					t.Fatalf("%s, source error after %d bytes: err=%v, want the source error", m.name, cut, err)
				}
			}
		}
	})
}

// Info/Xing/VBRI 首帧不是音频:未确认标记随这一帧一起丢弃,不留给其后的帧。
func TestMetaFirstFrameDropsUnconfirmedMark(t *testing.T) {
	sourceErr := errors.New("source reset")
	lame := readCorpus(t, "pluck-stereo-44k1-abr128-lame.mp3")
	starts, _ := audioFrames(t, lame)
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"Info", lame[:starts[0]]}, // 音频帧之前恰是 Info 帧
		{"VBRI", vbriStream(2, 2)[:417]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDecoder(context.Background(), &dataWithErrReader{r: bytes.NewReader(tc.data), err: sourceErr}, Strict())
			if err != nil {
				t.Fatal(err)
			}
			if d.fr.unconfirmed {
				t.Fatal("the unconfirmed mark outlived the tag frame it described")
			}
		})
	}
}

// 帧后的 4 字节已经不是帧头时,候选不再按未确认接受:标签探测途中源出错,与流结束一样
// 候选不成立,报告源错误。
func TestFirstFrameSourceErrorAfterNonHeader(t *testing.T) {
	data := cat(repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 1), make([]byte, 8))
	for _, m := range bothModes {
		t.Run("source error/"+m.name, func(t *testing.T) {
			sourceErr := errors.New("source reset")
			d, err := NewDecoder(context.Background(), &dataWithErrReader{r: bytes.NewReader(data), err: sourceErr}, m.opts...)
			if d != nil || !errors.Is(err, sourceErr) {
				t.Fatalf("decoder=%t err=%v, want no decoder and the source error", d != nil, err)
			}
		})
		t.Run("end of stream/"+m.name, func(t *testing.T) {
			d, err := NewDecoder(context.Background(), bytes.NewReader(data), m.opts...)
			if d != nil || !errors.Is(err, ErrNoAudio) {
				t.Fatalf("decoder=%t err=%v, want no decoder and ErrNoAudio", d != nil, err)
			}
		})
	}
}

// 源在未经确认的候选之后出错:候选带 emphasis 时多半是伪同步,两种模式都不交出它,
// 报告源错误而不是格式错误;没有 emphasis 的候选照常先交出。
func TestFirstFrameUnconfirmedWithEmphasis(t *testing.T) {
	for _, tc := range []struct {
		name     string
		emphasis int
		samples  int // 源错误之前交付的样本数
	}{
		{"no emphasis", 0, 1152 * 2},
		{"emphasis", 1, 0},
	} {
		frame := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, tc.emphasis), 417, 1)
		for _, m := range bothModes {
			t.Run(tc.name+"/"+m.name, func(t *testing.T) {
				sourceErr := errors.New("source reset")
				d, pcm, err := decodeMode(t, &dataWithErrReader{r: bytes.NewReader(frame), err: sourceErr}, m.opts...)
				if !errors.Is(err, sourceErr) || len(pcm) != tc.samples || (d != nil) != (tc.samples > 0) {
					t.Fatalf("decoder=%t, %d samples, err=%v, want the source error after %d samples", d != nil, len(pcm), err, tc.samples)
				}
			})
		}
	}
}
