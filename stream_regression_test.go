package gl3d

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
)

// 每次只交付一个字节,断言构造器在首帧与下一帧头到达后立即返回,
// 而不是等待固定的 4 KiB 扫描窗口。无需计时或依赖 goroutine 调度。
func TestSyncSearchReadsOnlyConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    [4]byte
		size int
	}{
		{"MPEG1 128kbps", mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417},
		{"MPEG25 16kbps", mkHeader(0, 1, 1, 2, 2, 0, 0, 0, 0), 144},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := repeatFrames(tc.h, tc.size, 40)
			r := bytes.NewReader(data)
			if _, err := NewDecoder(context.Background(), &oneByteReader{r: r}); err != nil {
				t.Fatal(err)
			}
			if n := len(data) - r.Len(); n != tc.size+4 {
				t.Fatalf("read %d bytes, want exactly %d for the frame and next header", n, tc.size+4)
			}
		})
	}
}

// finalErrorReader 真正将最后一批数据与指定错误同返,区别于 bytes.Reader
// 先返回 (n, nil)、下次调用才返回 (0, io.EOF) 的行为。
type finalErrorReader struct {
	data  []byte
	chunk int
	err   error
}

func (r *finalErrorReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := copy(p[:min(len(p), r.chunk)], r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

// 完整短流,包括只有一帧的输入,遇到非 EOF 源错误仍须交付全部完整帧;
// 不完整的下一帧头不得被吞掉或将源错误改成 EOF/格式错误。
func TestShortStreamSourceErrorPreservesFrames(t *testing.T) {
	h := mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0)
	srcErr := errors.New("source reset")
	for _, frames := range []int{1, 3} {
		for _, tail := range []int{0, 1, 3} {
			data := append(repeatFrames(h, 417, frames), h[:tail]...)
			for _, chunk := range []int{1, 7, 4096} {
				t.Run(fmt.Sprintf("frames%d/tail%d/chunk%d", frames, tail, chunk), func(t *testing.T) {
					d, err := drain(t, &finalErrorReader{data: data, chunk: chunk, err: srcErr})
					if !errors.Is(err, srcErr) {
						t.Fatalf("err=%v, want the original source error", err)
					}
					if d == nil {
						t.Fatal("NewDecoder discarded the complete frames")
					}
					if got := d.Frames(); got != int64(frames*1152) {
						t.Fatalf("Frames()=%d, want %d", got, frames*1152)
					}
				})
			}
		}
	}
	// 不完整的首帧不能作为可交付音频,源错误也须保留。
	partial := repeatFrames(h, 417, 1)[:200]
	if _, err := NewDecoder(context.Background(), &finalErrorReader{data: partial, chunk: 7, err: srcErr}); !errors.Is(err, srcErr) {
		t.Fatalf("partial first frame: err=%v, want the source error", err)
	}
}

// 孤立的范围外格式头不应使短流中的合法帧因探测窗口凑不满而被源错误吞掉。
func TestSourceErrorAfterIsolatedUnsupportedHeader(t *testing.T) {
	layer2 := mkHeader(3, 2, 1, 10, 0, 0, 0, 0, 0)
	data := append(layer2[:], make([]byte, 100)...)
	data = append(data, repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 2)...)
	srcErr := errors.New("source reset")
	d, err := drain(t, &finalErrorReader{data: data, chunk: 7, err: srcErr})
	if !errors.Is(err, srcErr) || d == nil {
		t.Fatalf("initialized=%v err=%v, want a decoder and the source error", d != nil, err)
	}
	if got := d.Frames(); got != 2*1152 {
		t.Fatalf("Frames()=%d, want 2304", got)
	}
}

// 单帧后接 ID3v1 的各种结尾。只有一帧时,首帧要么由帧头到齐前的流结束确认,要么由
// 恰好以流结束收尾的完整 ID3v1 确认:3 字节标识不足以排除伪同步,残缺标签或标签后另有
// 数据时两种模式都找不到音频。帧尾之后是无法识别的数据时,严格模式不交付这一帧,
// 宽松模式交付并记入报告。
func TestSingleFrameID3v1Boundary(t *testing.T) {
	frame := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 1)
	for _, tc := range []struct {
		name   string
		tail   []byte
		want   string // 严格模式的结果
		frames int64  // 严格模式交付的采样帧数
	}{
		{"no tag", nil, "ok", 1152},
		{"one byte", []byte("T"), "corrupt", 0},
		{"two bytes", []byte("TA"), "corrupt", 0},
		{"magic only", id3v1Tag()[:3], "truncated", 1152},
		{"partial tag", id3v1Tag()[:127], "noaudio", 0},
		{"complete tag", id3v1Tag(), "ok", 1152},
		{"tag then junk", append(id3v1Tag(), 1), "noaudio", 0},
	} {
		for _, chunk := range []int{1, 4096} {
			t.Run(fmt.Sprintf("%s/chunk%d", tc.name, chunk), func(t *testing.T) {
				data := append(append([]byte(nil), frame...), tc.tail...)
				d, err := drain(t, &finalErrorReader{data: data, chunk: chunk, err: io.EOF})
				if got := errClass(err); got != tc.want {
					t.Fatalf("strict: err=%v (%s), want %s", err, got, tc.want)
				}
				if tc.want == "noaudio" {
					if d != nil {
						t.Fatal("strict: ErrNoAudio from ReadPCM, want it from NewDecoder")
					}
				} else if got := d.Frames(); got != tc.frames {
					t.Fatalf("strict: Frames()=%d, want %d", got, tc.frames)
				}
				d, _, err = decodeMode(t, &finalErrorReader{data: data, chunk: chunk, err: io.EOF})
				if tc.want == "noaudio" {
					if !errors.Is(err, ErrNoAudio) || d != nil {
						t.Fatalf("lenient: err=%v, want ErrNoAudio from NewDecoder", err)
					}
					return
				}
				if err != nil || d.Frames() != 1152 || d.Report().Clean() != (tc.want == "ok") {
					t.Fatalf("lenient: Frames()=%d err=%v clean=%v, want 1152, no error and clean only without an anomaly", d.Frames(), err, d.Report().Clean())
				}
			})
		}
	}
}

func TestLAMEEncoderField(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"", false},
		{"         ", false},
		{"LAME\x003.1", false},
		{"LAME\n3.1", false},
		{"LAME\xff3.1", false},
		{"LAME3.100", true},
		{"Lavf61.7", true},
		{"Lavc62.11", true},
		{"GOGO3.13", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := make([]byte, 36)
			copy(b, tc.name)
			l, ok := parseLAMETag(b)
			if ok != tc.want {
				t.Fatalf("parseLAMETag(%q) ok=%v, want %v", tc.name, ok, tc.want)
			}
			if ok && l.encoder != tc.name {
				t.Fatalf("encoder=%q, want %q", l.encoder, tc.name)
			}
		})
	}
}

// 无 LAME 扩展的 Xing 帧仍有可信帧数;全零的帧尾填充不应被当成一个
// CRC 错误的 LAME 扩展,使帧数声明一起消失。
func TestXingWithoutLAMEPreservesFrameCount(t *testing.T) {
	for _, declared := range []int{3, 100} {
		t.Run(fmt.Sprintf("declared%d", declared), func(t *testing.T) {
			data := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 4)
			copy(data[36:], "Xing")
			binary.BigEndian.PutUint32(data[40:], 1)
			binary.BigEndian.PutUint32(data[44:], uint32(declared))
			d, err := drain(t, bytes.NewReader(data))
			if d == nil {
				t.Fatalf("NewDecoder failed: %v", err)
			}
			if d.xing == nil || d.xing.frames == nil || *d.xing.frames != uint32(declared) || d.Trimmed() {
				t.Fatalf("Xing=%+v Trimmed()=%v, want count %d without trimming", d.xing, d.Trimmed(), declared)
			}
			want := "ok"
			if declared != 3 {
				want = "truncated"
			}
			if got := errClass(err); got != want || d.Frames() != 3*1152 {
				t.Fatalf("frames=%d err=%v (%s), want 3456 frames and %s", d.Frames(), err, got, want)
			}
		})
	}
}
