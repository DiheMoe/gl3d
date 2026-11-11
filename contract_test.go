package gl3d

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

// audioFrames 返回各音频帧的起始偏移(不含 Xing/VBRI 标签帧)与最后一个完整
// 帧的结束偏移,仅作为构造截断与变异输入的测试夹具。
func audioFrames(t *testing.T, data []byte) (starts []int, end int) {
	t.Helper()
	p := 0
	if len(data) >= 10 && string(data[:3]) == "ID3" {
		size, footer, err := parseID3v2Header([10]byte(data[:10]))
		if err != nil {
			t.Fatal(err)
		}
		p = size
		if footer {
			p += 10
		}
	}
	for first := true; p+4 <= len(data); first = false {
		h, err := parseFrameHeader([4]byte(data[p : p+4]))
		if err != nil || p+h.frameSize > len(data) {
			break
		}
		if first {
			if m, _ := metaMagic(data[p:p+h.frameSize], h); m != "" {
				p += h.frameSize
				continue
			}
		}
		starts = append(starts, p)
		p += h.frameSize
	}
	return starts, p
}

// 缓冲契约:零长度请求返回 (0, nil) 且不推进。缓冲不足一个采样帧(每声道
// 一个样本)时返回 ErrSmallBuffer,同样不推进。
func TestReadPCMBufferContract(t *testing.T) {
	ctx := context.Background()
	d, err := NewDecoder(ctx, bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")), Strict())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if n, err := d.ReadPCM(ctx, nil); n != 0 || err != nil {
			t.Fatalf("zero-length request: (%d, %v), want (0, nil)", n, err)
		}
	}
	if n, err := d.ReadPCM(ctx, make([]float64, 1)); n != 0 || !errors.Is(err, ErrSmallBuffer) {
		t.Fatalf("1-sample buffer for stereo: (%d, %v), want (0, ErrSmallBuffer)", n, err)
	}
	if d.DecodedFrames() != 0 {
		t.Fatalf("DecodedFrames()=%d after non-advancing calls, want 0", d.DecodedFrames())
	}
	buf := make([]float64, 4096)
	var total int64
	for {
		n, err := d.ReadPCM(ctx, buf)
		total += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	// 220500 = 193×1152 − (576+529) − (1260−529),裁剪后恰为 5 s × 44100 Hz。
	// LAME 标签的 enc_delay 与 enc_padding 都要计入解码器 529 样本的结构延迟
	if total != 220500 || d.Frames() != 220500 {
		t.Fatalf("delivered %d, Frames()=%d, want 220500", total, d.Frames())
	}
}

// 解码中途取消时,必须及时(内存输入下远快于 100 ms)返回 context 错误并保持
// 该错误,不能退化为正常结束。
func TestReadPCMCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d, err := NewDecoder(ctx, bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")), Strict())
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]float64, 2304)
	if _, err := d.ReadPCM(ctx, buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	decodedAtCancel := d.DecodedFrames()
	start := time.Now()
	for {
		_, err = d.ReadPCM(ctx, buf)
		if err != nil {
			break
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("returned %v after cancel, want ≤ 100 ms", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if _, err := d.ReadPCM(ctx, buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("after cancel: err=%v, want context.Canceled again", err)
	}
	// 取消在帧之间检查,所以已解码待交付的样本可以交付,但不再解码新帧
	if d.DecodedFrames() != decodedAtCancel {
		t.Fatalf("decoded %d more sample frames after cancel, want 0", d.DecodedFrames()-decodedAtCancel)
	}
}

// cancelAfterReader 在累计读出 limit 字节后取消 context,模拟标签跳过途中的取消。
type cancelAfterReader struct {
	r      io.Reader
	limit  int
	read   int
	cancel context.CancelFunc
}

func (c *cancelAfterReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.read += n; c.read >= c.limit {
		c.cancel()
	}
	return n, err
}

// 大标签分块跳过的块间必须检查取消。取消后跳过立即停止,不得继续读完整个标签。
func TestCancelDuringTagSkip(t *testing.T) {
	data := append(id3v2Tag(3<<20), repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 2)...)
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelAfterReader{r: bytes.NewReader(data), limit: 256 << 10, cancel: cancel}
	_, err := NewDecoder(ctx, r, Strict())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if r.read > 1<<20 {
		t.Fatalf("read %d bytes after cancel at 256 KiB, want skipping to stop", r.read)
	}
}

// vbriStream 返回首帧带 VBRI 头(声明 declared 帧)、其后 actual 个静音帧的流。
func vbriStream(declared, actual int) []byte {
	h := mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0) // MPEG-1 128k 44.1k 立体声:417 B
	first := make([]byte, 417)
	copy(first, h[:])
	v := first[36:] // VBRI 固定位于帧头后 32 字节
	copy(v, "VBRI")
	binary.BigEndian.PutUint16(v[4:], 1)                       // version
	binary.BigEndian.PutUint16(v[8:], 75)                      // quality
	binary.BigEndian.PutUint32(v[10:], uint32(417*(actual+1))) // bytes
	binary.BigEndian.PutUint32(v[14:], uint32(declared))       // frames
	binary.BigEndian.PutUint16(v[20:], 1)                      // toc scale
	binary.BigEndian.PutUint16(v[22:], 2)                      // toc entry size
	return append(first, repeatFrames(h, 417, actual)...)
}
