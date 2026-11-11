package gl3d

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// lenient 选择宽松模式。宽松模式下,可以容忍的异常结构跳过并记入报告,不终止解码。
func lenient() Option {
	return func(c *config) { c.strict = false }
}

// decodeMode 以给定选项解码到流结束,返回解码器、已交付的 PCM 与终止错误
// (正常结束为 nil)。NewDecoder 失败时解码器为 nil。
func decodeMode(t *testing.T, r io.Reader, opts ...Option) (*Decoder, []float64, error) {
	t.Helper()
	d, err := NewDecoder(context.Background(), r, opts...)
	if err != nil {
		return nil, nil, err
	}
	var pcm []float64
	buf := make([]float64, 4096)
	for {
		n, err := d.ReadPCM(context.Background(), buf)
		pcm = append(pcm, buf[:n*d.Channels()]...)
		if err == io.EOF {
			return d, pcm, nil
		}
		if err != nil {
			return d, pcm, err
		}
		if n == 0 {
			t.Fatal("no progress: (0, nil) loop")
		}
	}
}

// 选项按传入顺序生效,后者覆盖前者。nil 选项被忽略,不传选项即宽松模式。
// 模式用可观察的行为判定。音频之后的垃圾在宽松模式下跳过并记入报告,
// 在严格模式下报 CorruptError。
func TestOptionsApplyInOrder(t *testing.T) {
	data := cat(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3"), noSync(300))
	strictOf := func(t *testing.T, opts ...Option) bool {
		t.Helper()
		d, pcm, err := decodeMode(t, bytes.NewReader(data), opts...)
		var c *CorruptError
		if errors.As(err, &c) {
			return true
		}
		if err != nil {
			t.Fatalf("err=%v, want nil or CorruptError", err)
		}
		if rep := d.Report(); len(pcm) != 220500*2 || rep.Clean() || rep.First.Kind != IssueCorrupt {
			t.Fatalf("delivered %d samples, report %+v", len(pcm), rep)
		}
		return false
	}
	for _, tc := range []struct {
		name   string
		opts   []Option
		strict bool
	}{
		{"no options", nil, false},
		{"Strict", []Option{Strict()}, true},
		{"lenient", []Option{lenient()}, false},
		{"lenient then Strict", []Option{lenient(), Strict()}, true},
		{"Strict then lenient", []Option{Strict(), lenient()}, false},
		{"Strict then nil", []Option{Strict(), nil}, true}, // nil 不重置此前的选项
		{"nil only", []Option{nil}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strictOf(t, tc.opts...); got != tc.strict {
				t.Fatalf("strict=%v, want %v", got, tc.strict)
			}
		})
	}
}
