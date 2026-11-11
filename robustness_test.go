package gl3d

import (
	"bytes"
	"errors"
	"testing"
)

// errClass 把解码的终止错误归入五类:无音频、不支持、截断、损坏、资源限制。
// 不在这五类中的错误返回 "other: " 加错误文本,会被调用方判为未分类。
func errClass(err error) string {
	var (
		u *UnsupportedError
		t *TruncatedError
		c *CorruptError
		l *LimitError
	)
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNoAudio):
		return "noaudio"
	case errors.As(err, &u):
		return "unsupported"
	case errors.As(err, &t):
		return "truncated"
	case errors.As(err, &c):
		return "corrupt"
	case errors.As(err, &l):
		return "limit"
	}
	return "other: " + err.Error()
}

// 在短样例的每个字节位置截断,按截断点所在的结构断言结果。帧或标签内部不完整
// 为截断。无可信总长度声明时,恰在帧边界结束是合法短流。有 Xing 帧数声明时,
// 少帧仍是截断。结构位置取自 ffprobe 的包位置与 ID3 头。
func TestTruncationAtEveryByte(t *testing.T) {
	cases := []struct {
		file string
		want func(n int) (class string, frames int64)
	}{
		{
			// ID3v2.4 标签 [0,44),Info 帧 [44,670) 声明 3 帧,音频帧起于 670/1296/1923,止于 2550
			"short-tone-stereo-44k1-cbr192.mp3",
			func(n int) (string, int64) {
				switch {
				case n <= 2: // 空输入或 "I"/"ID" 都不足以识别为标签
					return "noaudio", 0
				case n < 44:
					return "truncated", 0
				case n == 44: // 只有标签
					return "noaudio", 0
				case n < 2550:
					return "truncated", 0
				}
				return "ok", 2205
			},
		},
		{
			// 无标签,30 个 144 B 的帧(包位置 0,144,…,4176)
			"pluck-stereo-8k-cbr16-lame.mp3",
			func(n int) (string, int64) {
				switch {
				case n == 0:
					return "noaudio", 0
				case n%144 == 0:
					return "ok", int64(n/144) * 576
				}
				return "truncated", 0
			},
		},
	}
	for _, tc := range cases {
		data := readCorpus(t, tc.file)
		bad := 0
		for n := 0; n <= len(data); n++ {
			wantClass, wantFrames := tc.want(n)
			d, err := drain(t, bytes.NewReader(data[:n]))
			got := errClass(err)
			if got == wantClass && (got != "ok" || d.Frames() == wantFrames) {
				continue
			}
			if bad++; bad <= 5 {
				t.Errorf("%s[:%d]: %s (%v), want %s", tc.file, n, got, err, wantClass)
			}
		}
		if bad > 5 {
			t.Errorf("%s: %d truncation points misclassified in total", tc.file, bad)
		}
	}
}

// 两个样例的每个截断点都经过 streamOnce:两种模式、整体与分块读取的结果一致,
// 模式等价成立,宽松模式不以截断或损坏结束,输出有限。TestTruncationAtEveryByte
// 断言严格模式的错误类别,这里检查两种模式之间的一致性。
func TestTruncationSweepModes(t *testing.T) {
	if testing.Short() {
		t.Skip("每个截断点解码四遍,-short 下跳过")
	}
	for _, name := range []string{"short-tone-stereo-44k1-cbr192.mp3", "pluck-stereo-8k-cbr16-lame.mp3"} {
		t.Run(name, func(t *testing.T) {
			data := readCorpus(t, name)
			for n := 0; n <= len(data); n++ {
				func() {
					defer func() {
						if t.Failed() {
							t.Logf("truncated at byte %d of %d", n, len(data))
						}
					}()
					withLimits(t, func() { streamOnce(t, data[:n]) })
				}()
			}
		})
	}
}

// 定向变异:window_switching 置位而 block_type 为 0 是保留组合,必须报 CorruptError
// 并定位到帧序号。位置按 MPEG-1 立体声 side info 布局:gr0/ch0 的 big_values 32–40、
// scalefac_compress 49–52、window_switching 53、block_type 54–55。
// big_values 与 scalefac_compress 置 0,该 granule 其余字段无论怎样解释都能解完,
// 错误只能来自对保留组合的检查。
func TestDirectedMutations(t *testing.T) {
	const target = 10
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	starts, _ := audioFrames(t, data)
	si := data[starts[target]+4:]
	setBits(si, 32, 9, 0)
	setBits(si, 49, 4, 0)
	setBits(si, 53, 1, 1)
	setBits(si, 54, 2, 0)
	_, err := drain(t, bytes.NewReader(data))
	if got := errClass(err); got != "corrupt" {
		t.Fatalf("%s (%v), want corrupt", got, err)
	}
	var c *CorruptError
	if errors.As(err, &c) && c.Frame != target {
		t.Fatalf("CorruptError.Frame=%d, want %d (%v)", c.Frame, target, err)
	}
}
