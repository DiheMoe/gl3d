package gl3d

import (
	"bytes"
	"errors"
	"testing"
)

// errClass 把解码的终止错误归入五类:无音频、不支持、截断、损坏、资源限制;
// 不在这五类中的错误原样返回文本,会被调用方判为未分类。
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

// 在短样例的每个字节位置截断,按截断点所在的结构断言结果:帧或标签内部不完整
// 为截断;无可信总长度声明时恰在帧边界结束是合法短流;有 Xing 帧数声明时少帧
// 仍是截断。结构位置取自 ffprobe 的包位置与 ID3 头。
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
				case n <= 2: // 空输入,或 "I"/"ID":不足以识别为标签
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

// 定向变异:标签长度、side information、reservoir 与尾部数据的结构损坏各归入
// 确定的错误类别,损坏帧定位到帧序号,不 panic、不死循环。
// 位置按 MPEG-1 立体声 side info 布局:main_data_begin 0–8,gr0/ch0 的
// part2_3_length 20–31、big_values 32–40、scalefac_compress 49–52、window_switching 53,
// 其后 block_type 54–55(切换块)或 table_select 54–68(普通块),
// 四个 granule/声道的 part2_3_length 依次起于 20、79、138、197。
func TestDirectedMutations(t *testing.T) {
	const target = 10
	sideInfo := func(data []byte, frame int) []byte {
		starts, _ := audioFrames(t, data)
		return data[starts[frame]+4:]
	}
	cases := []struct {
		name   string
		mutate func(data []byte) []byte
		class  string
		frame  int64 // CorruptError 的期望帧序号(-1 表示不检查)
	}{
		{"id3v2 size not syncsafe", func(d []byte) []byte { d[9] |= 0x80; return d }, "corrupt", -1},
		{"id3v2 size beyond tag budget", func(d []byte) []byte { copy(d[6:10], []byte{0x7F, 0x7F, 0x7F, 0x7F}); return d }, "limit", -1},
		{"main_data_begin beyond empty reservoir", func(d []byte) []byte { setBits(sideInfo(d, 0), 0, 9, 1); return d }, "corrupt", 0},
		// 三个区都用不占位的表 0:缺少 big_values 上限检查时谱线下标会越过 576
		{"big_values above 288", func(d []byte) []byte {
			si := sideInfo(d, target)
			setBits(si, 32, 9, 511)
			setBits(si, 53, 1, 0)
			setBits(si, 54, 15, 0)
			return d
		}, "corrupt", target},
		// big_values 与 scalefac_compress 置 0,该 granule 其余字段无论怎样解释都能
		// 解完,错误只能来自对保留组合的检查
		{"window switching with block_type 0", func(d []byte) []byte {
			si := sideInfo(d, target)
			setBits(si, 32, 9, 0)
			setBits(si, 49, 4, 0)
			setBits(si, 53, 1, 1)
			setBits(si, 54, 2, 0)
			return d
		}, "corrupt", target},
		{"part2_3_length beyond main data", func(d []byte) []byte {
			si := sideInfo(d, target)
			for _, pos := range []int{20, 79, 138, 197} {
				setBits(si, pos, 12, 4095)
			}
			return d
		}, "corrupt", target},
		{"garbage after last frame", func(d []byte) []byte { return append(d, "not a frame"...) }, "corrupt", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.mutate(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3"))
			_, err := drain(t, bytes.NewReader(data))
			if got := errClass(err); got != tc.class {
				t.Fatalf("%s (%v), want %s", got, err, tc.class)
			}
			var c *CorruptError
			if tc.frame >= 0 && errors.As(err, &c) && c.Frame != tc.frame {
				t.Fatalf("CorruptError.Frame=%d, want %d (%v)", c.Frame, tc.frame, err)
			}
		})
	}
}
