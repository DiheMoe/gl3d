package gl3d

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

// BenchmarkDecode 的度量口径:单核(以 -cpu 1 运行)、输入预读入内存、只做解码
// 不做响度分析;x-realtime 为音频时长与解码耗时之比。
func BenchmarkDecode(b *testing.B) {
	benchmarkPCM(b, false)
}

func BenchmarkNextPCM(b *testing.B) {
	benchmarkPCM(b, true)
}

func benchmarkPCM(b *testing.B, borrowed bool) {
	for _, name := range []string{
		"sine440-stereo-44k1-cbr192",        // MPEG-1 立体声 MS,长块为主
		"sine440-mono-44k1-cbr96",           // MPEG-1 单声道
		"sine330-stereo-22k05-cbr64",        // MPEG-2
		"pluck-stereo-48k-cbr128-lame",      // 短块与表 0 区域
		"sine440-stereo-44k1-cbr192-shine",  // 第二编码器,无标签
		"dense5min-stereo-44k1-cbr128-lame", // 五分钟密集频谱,降低初始化占比
	} {
		data, err := os.ReadFile("corpus/synthetic/" + name + ".mp3")
		if err != nil {
			b.Fatal(err)
		}
		b.Run(name, func(b *testing.B) {
			var buf []float64
			if !borrowed {
				buf = make([]float64, 8192)
			}
			var seconds float64
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				d, err := NewDecoder(context.Background(), bytes.NewReader(data))
				if err != nil {
					b.Fatal(err)
				}
				for {
					if borrowed {
						_, err = d.NextPCM(context.Background())
					} else {
						_, err = d.ReadPCM(context.Background(), buf)
					}
					if err == io.EOF {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				seconds = float64(d.Frames()) / float64(d.SampleRate())
			}
			b.ReportMetric(seconds*float64(b.N)/b.Elapsed().Seconds(), "x-realtime")
		})
	}
}
