package gl3d

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// lenient 选择宽松模式:可以容忍的异常结构跳过并记入报告,不终止解码。
func lenient() Option {
	return func(c *config) { c.strict = false }
}

// decodeMode 以给定选项解码到流结束,返回解码器、已交付的 PCM 与终止错误
// (正常结束为 nil);NewDecoder 失败时解码器为 nil。
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

// 选项按传入顺序生效,后者覆盖前者;nil 选项被忽略,结果与不传选项相同。
func TestOptionsApplyInOrder(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	strictOf := func(t *testing.T, opts ...Option) bool {
		t.Helper()
		d, err := NewDecoder(context.Background(), bytes.NewReader(data), opts...)
		if err != nil {
			t.Fatal(err)
		}
		return d.fr.strict
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strictOf(t, tc.opts...); got != tc.strict {
				t.Fatalf("strict=%v, want %v", got, tc.strict)
			}
		})
	}
	t.Run("nil option", func(t *testing.T) {
		if got, want := strictOf(t, []Option{nil}...), strictOf(t); got != want {
			t.Fatalf("strict=%v with a nil option, %v without options", got, want)
		}
	})
}

// 不传选项即宽松模式:音频之后的垃圾跳过并记入报告,不终止解码;传 Strict() 则报
// CorruptError。
func TestDefaultModeIsLenient(t *testing.T) {
	data := cat(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3"), noSync(300))
	d, pcm, err := decodeMode(t, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("default mode: %v", err)
	}
	rep := d.Report()
	if len(pcm) != 220500*2 || rep.Clean() || rep.First.Kind != IssueCorrupt {
		t.Fatalf("default mode delivered %d samples, report %+v", len(pcm), rep)
	}
	_, _, err = decodeMode(t, bytes.NewReader(data), Strict())
	var c *CorruptError
	if !errors.As(err, &c) {
		t.Fatalf("Strict(): err=%v, want CorruptError for trailing garbage", err)
	}
}
