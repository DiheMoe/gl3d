package gl3d

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"
)

// chunkReader 按伪随机长度(1..max)分块返回数据;最后一块与 io.EOF 同返——Reader 的合法风格,解码不得依赖"先数据后 EOF"。
type chunkReader struct {
	data []byte
	rng  *rand.Rand
	max  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(1+c.rng.Intn(c.max), len(p), len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	if len(c.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

// stutterReader 隔次返回 (0, nil):偶发无进展是 Reader 的合法行为,解码须容忍并继续(区别于持续无进展的报错)。
type stutterReader struct {
	r    io.Reader
	idle bool
}

func (s *stutterReader) Read(p []byte) (int, error) {
	if s.idle = !s.idle; s.idle {
		return 0, nil
	}
	return s.r.Read(p)
}

type decodeResult struct {
	pcm                []float64
	frames, decoded    int64
	trimmed            bool
	err                error
	sampleRate, nchans int
}

// decodeWith 以 bufFrames 个采样帧的输出缓冲完整解码 r;错误存入返回值而非 t.Fatal,供用例断言。
func decodeWith(t *testing.T, r io.Reader, bufFrames int) decodeResult {
	t.Helper()
	ctx := context.Background()
	d, err := NewDecoder(ctx, r)
	if err != nil {
		return decodeResult{err: err}
	}
	res := decodeResult{sampleRate: d.SampleRate(), nchans: d.Channels()}
	buf := make([]float64, bufFrames*d.Channels())
	for {
		n, err := d.ReadPCM(ctx, buf)
		res.pcm = append(res.pcm, buf[:n*d.Channels()]...)
		if err != nil {
			if err != io.EOF {
				res.err = err
			}
			break
		}
		if n == 0 {
			t.Fatal("no progress: (0, nil)")
		}
	}
	res.frames, res.decoded, res.trimmed = d.Frames(), d.DecodedFrames(), d.Trimmed()
	return res
}

// 流式契约:同一输入无论以何种分块方式送达、用多大的输出缓冲读取,交付的
// PCM、计数与终止状态都必须逐位一致。
func TestStreamingInvarianceMatrix(t *testing.T) {
	files := []string{
		"sine440-stereo-44k1-cbr192.mp3",    // ID3v2 + Info 标签 + gapless 尾裁(尾部缓冲)
		"short-tone-stereo-44k1-cbr192.mp3", // 短输入,裁剪量占整体比例大
		"sine880-mono-11k025-cbr32.mp3",     // 单声道 MPEG-2.5
		"pluck-stereo-8k-cbr16-lame.mp3",    // 无 ID3/Xing 标签,144 B 小帧
	}
	readers := []struct {
		name string
		mk   func(data []byte) io.Reader
	}{
		{"bytes", func(b []byte) io.Reader { return bytes.NewReader(b) }},
		{"onebyte", func(b []byte) io.Reader { return &oneByteReader{r: bytes.NewReader(b)} }},
		{"chunks", func(b []byte) io.Reader { return &chunkReader{data: b, rng: rand.New(rand.NewSource(5)), max: 4096} }},
		{"smallchunks", func(b []byte) io.Reader { return &chunkReader{data: b, rng: rand.New(rand.NewSource(6)), max: 7} }},
		{"stutter", func(b []byte) io.Reader { return &stutterReader{r: bytes.NewReader(b)} }},
	}
	for _, file := range files {
		data := readCorpus(t, file)
		want := decodeWith(t, bytes.NewReader(data), 4096)
		if want.err != nil {
			t.Fatalf("%s: reference decode: %v", file, want.err)
		}
		for _, rd := range readers {
			for _, bufFrames := range []int{1, 7, 1152, 4096, 100000} {
				t.Run(fmt.Sprintf("%s/%s/buf%d", file, rd.name, bufFrames), func(t *testing.T) {
					got := decodeWith(t, rd.mk(data), bufFrames)
					if got.err != nil {
						t.Fatalf("err=%v", got.err)
					}
					if got.frames != want.frames || got.decoded != want.decoded || got.trimmed != want.trimmed {
						t.Fatalf("counts (%d, %d, %v), want (%d, %d, %v)",
							got.frames, got.decoded, got.trimmed, want.frames, want.decoded, want.trimmed)
					}
					if !bytes.Equal(float64Bytes(got.pcm), float64Bytes(want.pcm)) {
						t.Fatal("PCM differs from the reference decode")
					}
				})
			}
		}
	}
}

// 源在交付全部数据的同时返回非 EOF 错误:已解码样本照常交付。gapless 尾裁
// 只在正常结束时应用——流以错误告终,无法确认尾部是编码器填充,保留的尾部
// 样本一并交付;最终以该源错误结束。
func TestStreamingSourceErrorAtEnd(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	want := decodeWith(t, bytes.NewReader(data), 4096)
	srcErr := errors.New("connection reset")
	got := decodeWith(t, &dataWithErrReader{r: bytes.NewReader(data), err: srcErr}, 4096)
	if !errors.Is(got.err, srcErr) {
		t.Fatalf("err=%v, want the source error", got.err)
	}
	if len(got.pcm) < len(want.pcm) || !bytes.Equal(float64Bytes(got.pcm[:len(want.pcm)]), float64Bytes(want.pcm)) {
		t.Fatalf("delivered %d samples, want the %d samples of a clean decode as prefix", len(got.pcm), len(want.pcm))
	}
}

// 源错误发生在下一帧帧头读到一半时:之前的完整帧照常解码交付,最终报告该
// 源错误——半个帧头既不能当完整帧,也不能静默当作正常结尾。
func TestStreamingSourceErrorMidHeader(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	starts, _ := audioFrames(t, data)
	srcErr := errors.New("connection reset")
	got := decodeWith(t, &dataWithErrReader{r: bytes.NewReader(data[:starts[100]+2]), err: srcErr}, 4096)
	if !errors.Is(got.err, srcErr) {
		t.Fatalf("err=%v, want the source error", got.err)
	}
	if got.decoded != 100*1152 {
		t.Fatalf("DecodedFrames()=%d, want %d (100 个完整帧)", got.decoded, 100*1152)
	}
}

// starveReader 交付前 n 字节后永远返回 (0, nil),模拟解码中途的持续无进展。
type starveReader struct {
	r *bytes.Reader
	n int
}

func (s *starveReader) Read(p []byte) (int, error) {
	if s.n <= 0 {
		return 0, nil
	}
	if len(p) > s.n {
		p = p[:s.n]
	}
	m, err := s.r.Read(p)
	s.n -= m
	return m, err
}

// 中途持续无进展必须报 io.ErrNoProgress,而不是无限 (0, nil) 或伪装正常结束;已有解码产出证明饥饿发生在流中段。
func TestStreamingStarvedMidStream(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	got := decodeWith(t, &starveReader{r: bytes.NewReader(data), n: len(data) / 2}, 4096)
	if !errors.Is(got.err, io.ErrNoProgress) {
		t.Fatalf("err=%v, want io.ErrNoProgress", got.err)
	}
	if got.decoded == 0 {
		t.Fatal("decoded nothing before starvation")
	}
}
