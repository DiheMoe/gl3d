package gl3d

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"strings"
	"testing"
)

// oneByteReader 每次最多返回 1 字节:短读是 Reader 的合法行为,解码路径不得假设一次填满。
type oneByteReader struct{ r *bytes.Reader }

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

// dataWithErrReader 在数据耗尽处把 io.EOF 换成指定错误,模拟源失败:Reader 允许最后一批数据与错误同返。
type dataWithErrReader struct {
	r   *bytes.Reader
	err error
}

func (d *dataWithErrReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if err == io.EOF || n > 0 && d.r.Len() == 0 {
		err = d.err
	}
	return n, err
}

// zeroReader 永远返回 (0, nil):合法但无进展,解码器必须报错而不是空转。
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return 0, nil }

func readCorpus(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("corpus/synthetic/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNewDecoderCorpusGolden(t *testing.T) {
	t.Run("lame stereo with Info+LAME tag", func(t *testing.T) {
		d, err := NewDecoder(context.Background(), bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")))
		if err != nil {
			t.Fatal(err)
		}
		if d.SampleRate() != 44100 || d.Channels() != 2 {
			t.Fatalf("rate=%d ch=%d", d.SampleRate(), d.Channels())
		}
		if d.xing == nil || d.xing.frames == nil || *d.xing.frames != 193 {
			t.Fatalf("xing: %+v", d.xing)
		}
		if d.lame == nil || d.lame.encDelay != 576 || d.lame.encPadding != 1260 {
			t.Fatalf("lame: %+v", d.lame)
		}
		if d.header.version != MPEG1 {
			t.Fatalf("version %v", d.header.version)
		}
	})
	t.Run("shine no tags", func(t *testing.T) {
		d, err := NewDecoder(context.Background(), bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")))
		if err != nil {
			t.Fatal(err)
		}
		if d.SampleRate() != 44100 || d.Channels() != 2 || d.xing != nil || d.lame != nil {
			t.Fatalf("rate=%d ch=%d xing=%+v lame=%+v", d.SampleRate(), d.Channels(), d.xing, d.lame)
		}
	})
	t.Run("mpeg2.5 mono", func(t *testing.T) {
		d, err := NewDecoder(context.Background(), bytes.NewReader(readCorpus(t, "sine880-mono-11k025-cbr32.mp3")))
		if err != nil {
			t.Fatal(err)
		}
		if d.SampleRate() != 11025 || d.Channels() != 1 || d.header.version != MPEG25 {
			t.Fatalf("rate=%d ch=%d v=%v", d.SampleRate(), d.Channels(), d.header.version)
		}
		if d.xing == nil || *d.xing.frames != 98 {
			t.Fatalf("xing: %+v", d.xing)
		}
	})
}

func TestNewDecoderShortReads(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	d, err := NewDecoder(context.Background(), &oneByteReader{r: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	if d.SampleRate() != 44100 || d.xing == nil || *d.xing.frames != 193 {
		t.Fatalf("rate=%d xing=%+v", d.SampleRate(), d.xing)
	}
}

func TestNewDecoderErrors(t *testing.T) {
	frame := func() []byte {
		h := mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0) // 128 kbps 44.1 kHz 立体声:帧长 417 B
		return append(h[:], make([]byte, 417-4)...)
	}

	t.Run("junk beyond sync budget", func(t *testing.T) {
		data := append(make([]byte, 70<<10), frame()...)
		data = append(data, frame()...)
		_, err := NewDecoder(context.Background(), bytes.NewReader(data))
		var l *LimitError
		if !errors.As(err, &l) {
			t.Fatalf("err=%v, want LimitError", err)
		}
	})
	t.Run("junk within budget then frames", func(t *testing.T) {
		data := append(make([]byte, 100), frame()...)
		data = append(data, frame()...)
		d, err := NewDecoder(context.Background(), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if d.SampleRate() != 44100 {
			t.Fatalf("rate=%d", d.SampleRate())
		}
	})
	t.Run("truncated id3v2", func(t *testing.T) {
		data := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0x07, 0x7F} // 声明标签体 1023 字节(syncsafe)
		data = append(data, make([]byte, 50)...)
		_, err := NewDecoder(context.Background(), bytes.NewReader(data))
		var tr *TruncatedError
		if !errors.As(err, &tr) {
			t.Fatalf("err=%v, want TruncatedError", err)
		}
	})
	t.Run("only tags no audio", func(t *testing.T) {
		data := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 10}
		data = append(data, make([]byte, 10)...)
		_, err := NewDecoder(context.Background(), bytes.NewReader(data))
		if !errors.Is(err, ErrNoAudio) {
			t.Fatalf("err=%v, want ErrNoAudio", err)
		}
	})
	t.Run("empty input", func(t *testing.T) {
		_, err := NewDecoder(context.Background(), bytes.NewReader(nil))
		if !errors.Is(err, ErrNoAudio) {
			t.Fatalf("err=%v, want ErrNoAudio", err)
		}
	})
	t.Run("zero progress reader", func(t *testing.T) {
		_, err := NewDecoder(context.Background(), zeroReader{})
		if !errors.Is(err, io.ErrNoProgress) {
			t.Fatalf("err=%v, want io.ErrNoProgress", err)
		}
	})
	t.Run("source error preserved", func(t *testing.T) {
		myErr := errors.New("network reset")
		r := &dataWithErrReader{r: bytes.NewReader(make([]byte, 10)), err: myErr}
		_, err := NewDecoder(context.Background(), r)
		if !errors.Is(err, myErr) {
			t.Fatalf("err=%v, want wrapped source error", err)
		}
	})
	t.Run("context canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := NewDecoder(ctx, bytes.NewReader(frame()))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want context.Canceled", err)
		}
	})
	t.Run("false sync then real frames", func(t *testing.T) {
		data := []byte{0xFF, 0xFB, 0xF0, 0x00} // 假同步:同步位匹配但 bitrate index 15 非法
		data = append(data, frame()...)
		data = append(data, frame()...)
		d, err := NewDecoder(context.Background(), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if d.SampleRate() != 44100 {
			t.Fatalf("rate=%d", d.SampleRate())
		}
	})
	t.Run("single frame at EOF accepted", func(t *testing.T) {
		d, err := NewDecoder(context.Background(), bytes.NewReader(frame()))
		if err != nil {
			t.Fatal(err)
		}
		if d.SampleRate() != 44100 {
			t.Fatalf("rate=%d", d.SampleRate())
		}
	})
}

// repeatFrames 返回 n 个以 h 为帧头、帧体全零的 size 字节帧。
func repeatFrames(h [4]byte, size, n int) []byte {
	var out []byte
	for i := 0; i < n; i++ {
		out = append(out, h[:]...)
		out = append(out, make([]byte, size-4)...)
	}
	return out
}

func TestNewDecoderUnsupportedStreams(t *testing.T) {
	layer2 := mkHeader(3, 2, 1, 10, 0, 0, 0, 0, 0) // Layer II 192k 44.1k:帧长 626 B
	cases := []struct {
		name    string
		data    []byte
		feature string
	}{
		{"layer II", repeatFrames(layer2, 626, 10), "Layer II"},
		{"layer II beyond sync budget", repeatFrames(layer2, 626, 120), "Layer II"},
		{"layer I", repeatFrames(mkHeader(3, 3, 1, 8, 0, 0, 0, 0, 0), 276, 20), "Layer I"}, // 256k:帧长 276 B
		{"free format", repeatFrames(mkHeader(3, 1, 1, 0, 0, 0, 0, 0, 0), 500, 20), "free format"},
		{"emphasis", repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 1), 417, 20), "emphasis"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDecoder(context.Background(), bytes.NewReader(tc.data))
			var u *UnsupportedError
			if !errors.As(err, &u) {
				t.Fatalf("err=%v, want UnsupportedError", err)
			}
			if !strings.Contains(u.Feature, tc.feature) {
				t.Fatalf("Feature=%q, want mention of %q", u.Feature, tc.feature)
			}
		})
	}
}

// 首帧定位不能只匹配同步位:候选须通过帧头校验,并在数据充足时以其帧长处的
// 下一个有效帧头确认;逐字节推进的搜索以 64 KiB 预算为限。
func TestSyncSearchCandidates(t *testing.T) {
	layer3 := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 2)
	t.Run("isolated layer II header then layer III", func(t *testing.T) {
		h := mkHeader(3, 2, 1, 10, 0, 0, 0, 0, 0)
		data := append(append(h[:], make([]byte, 100)...), layer3...)
		d, err := NewDecoder(context.Background(), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if d.SampleRate() != 44100 {
			t.Fatalf("rate=%d", d.SampleRate())
		}
	})
	t.Run("0xFF fill beyond sync budget", func(t *testing.T) {
		// 连续 0xFF:每个字节都与下一字节凑成同步候选,逐字节推进也受预算限制
		data := append(bytes.Repeat([]byte{0xFF}, 70<<10), layer3...)
		_, err := NewDecoder(context.Background(), bytes.NewReader(data))
		var l *LimitError
		if !errors.As(err, &l) {
			t.Fatalf("err=%v, want LimitError", err)
		}
	})
}

// 同步搜索预算按首帧前丢弃的字节数计:超过 64 KiB 即资源超限,流随后结束也一样;
// 结果不随读取分块变化。
func TestSyncSearchBudgetAtStreamEnd(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"junk at budget", make([]byte, syncSearchBudget), "noaudio"},
		{"junk beyond budget", make([]byte, syncSearchBudget+1), "limit"},
		{"junk at budget then partial sync", append(make([]byte, syncSearchBudget), 0xFF), "truncated"},
		{"junk beyond budget then partial sync", append(make([]byte, syncSearchBudget+1), 0xFF), "limit"},
	}
	for _, tc := range cases {
		for name, r := range map[string]io.Reader{
			"bytes":   bytes.NewReader(tc.data),
			"onebyte": &oneByteReader{r: bytes.NewReader(tc.data)},
			"chunked": &chunkReader{data: tc.data, rng: rand.New(rand.NewSource(1)), max: 97},
		} {
			_, err := NewDecoder(context.Background(), r)
			if got := errClass(err); got != tc.want {
				t.Errorf("%s, %s reader: %s (%v), want %s", tc.name, name, got, err, tc.want)
			}
		}
	}
}

// id3v2Tag 返回声明 size 字节标签体(不含 10 字节头)的 ID3v2.4 标签;长度按 syncsafe(每字节 7 位)编码。
func id3v2Tag(size int) []byte {
	tag := []byte{'I', 'D', '3', 4, 0, 0,
		byte(size >> 21 & 0x7F), byte(size >> 14 & 0x7F), byte(size >> 7 & 0x7F), byte(size & 0x7F)}
	return append(tag, make([]byte, size)...)
}

// 大标签(如内嵌封面)可达数 MiB,远超内部缓冲:必须分块跳过,既不按声明
// 长度分配内存,也不因缓冲不足而失败。
func TestSkipLargeID3v2(t *testing.T) {
	frames := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 2)
	for _, size := range []int{100 << 10, 3 << 20} {
		data := append(id3v2Tag(size), frames...)
		for name, r := range map[string]io.Reader{
			"bytes":   bytes.NewReader(data),
			"onebyte": &oneByteReader{r: bytes.NewReader(data)},
		} {
			if name == "onebyte" && size > 1<<20 {
				continue // 逐字节读 3 MiB 太慢,bytes 读取已覆盖
			}
			d, err := NewDecoder(context.Background(), r)
			if err != nil {
				t.Fatalf("size %d %s: %v", size, name, err)
			}
			if d.SampleRate() != 44100 {
				t.Fatalf("size %d %s: rate=%d", size, name, d.SampleRate())
			}
		}
	}
}

func TestSkipID3v2Limits(t *testing.T) {
	t.Run("truncated large tag", func(t *testing.T) {
		data := id3v2Tag(200 << 10)[:150<<10]
		_, err := NewDecoder(context.Background(), bytes.NewReader(data))
		var tr *TruncatedError
		if !errors.As(err, &tr) {
			t.Fatalf("err=%v, want TruncatedError", err)
		}
	})
	t.Run("tags beyond 16 MiB budget", func(t *testing.T) {
		// 预算按累计计算:单个 9 MiB 标签合法,两个相加越过 16 MiB
		big := id3v2Tag(9 << 20)
		data := append(append(append([]byte{}, big...), big...), repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 2)...)
		_, err := NewDecoder(context.Background(), bytes.NewReader(data))
		var l *LimitError
		if !errors.As(err, &l) {
			t.Fatalf("err=%v, want LimitError", err)
		}
	})
}

// rawID3v2 组装 ID3v2.major 标签:10 字节头 + body;footer 为真时追加 v2.4 的
// "3DI" 尾部(协议要求此时 flags 含 0x10)。
func rawID3v2(major, flags byte, body []byte, footer bool) []byte {
	n := len(body)
	size := []byte{byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}
	out := append([]byte{'I', 'D', '3', major, 0, flags}, size...)
	out = append(out, body...)
	if footer {
		out = append(out, '3', 'D', 'I', major, 0, flags)
		out = append(out, size...)
	}
	return out
}

// 文件开头连续的 ID3v2.2/2.3/2.4 标签(含 v2.4 footer)都必须完整跳过,不影响解码结果。
func TestID3v2VersionsSkipped(t *testing.T) {
	audio := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3") // 无标签
	_, want := decodeAll(t, bytes.NewReader(audio))
	v22 := rawID3v2(2, 0, []byte("TT2\x00\x00\x05\x00gl3d"), false)
	v23 := rawID3v2(3, 0, []byte("TIT2\x00\x00\x00\x05\x00\x00\x00gl3d"), false)
	v24 := rawID3v2(4, 0, []byte("TIT2\x00\x00\x00\x05\x00\x00\x03gl3d"), false)
	v24f := rawID3v2(4, 0x10, []byte("TIT2\x00\x00\x00\x05\x00\x00\x03gl3d"), true)
	cases := map[string][]byte{
		"v2.2":        v22,
		"v2.3":        v23,
		"v2.4":        v24,
		"v2.4 footer": v24f,
		// footer 若未被跳过,其后 100 KiB 的标签会计入 64 KiB 同步预算,本用例随之失败
		"v2.4 footer then large tag": append(append([]byte{}, v24f...), id3v2Tag(100<<10)...),
	}
	for name, tag := range cases {
		t.Run(name, func(t *testing.T) {
			d, got := decodeAll(t, bytes.NewReader(append(append([]byte{}, tag...), audio...)))
			if d.Frames() != 220032 || !bytes.Equal(float64Bytes(got), float64Bytes(want)) {
				t.Fatalf("Frames()=%d, PCM equal=%v; want 220032 and identical PCM",
					d.Frames(), bytes.Equal(float64Bytes(got), float64Bytes(want)))
			}
		})
	}
}

func TestSkipMultipleID3v2(t *testing.T) {
	tag := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 4}
	tag = append(tag, make([]byte, 4)...)
	hdr := mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0)
	frame := append(hdr[:], make([]byte, 413)...)
	data := append(append(append([]byte{}, tag...), tag...), frame...)
	data = append(data, frame...)
	d, err := NewDecoder(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if d.SampleRate() != 44100 {
		t.Fatalf("rate=%d", d.SampleRate())
	}
}
