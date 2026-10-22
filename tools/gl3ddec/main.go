// Command gl3ddec 用 gl3d 解码 MP3 文件,把交错的 float64 小端 PCM 写入 OUT,并以
// JSON 输出格式与采样帧计数。PCM 布局与 refgen 写出的 FFmpeg 参考相同,供 pcmdiff
// 比较。
//
// 用法:gl3ddec INPUT.mp3 OUT.f64le
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"

	gl3d "github.com/DiheMoe/gl3d"
)

type summary struct {
	Input         string `json:"input"`
	SampleRate    int    `json:"sampleRate"`
	Channels      int    `json:"channels"`
	Frames        int64  `json:"frames"`
	DecodedFrames int64  `json:"decodedFrames"`
	Trimmed       bool   `json:"trimmed"`
}

func run(in, out string) (*summary, error) {
	src, err := os.Open(in)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	dst, err := os.Create(out)
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	ctx := context.Background()
	d, err := gl3d.NewDecoder(ctx, bufio.NewReader(src))
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriter(dst)
	buf := make([]float64, 8192*d.Channels())
	var b [8]byte
	for {
		n, err := d.ReadPCM(ctx, buf)
		for _, v := range buf[:n*d.Channels()] {
			binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
			w.Write(b[:])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	return &summary{
		Input:         in,
		SampleRate:    d.SampleRate(),
		Channels:      d.Channels(),
		Frames:        d.Frames(),
		DecodedFrames: d.DecodedFrames(),
		Trimmed:       d.Trimmed(),
	}, dst.Close()
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gl3ddec INPUT.mp3 OUT.f64le")
		os.Exit(2)
	}
	s, err := run(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "gl3ddec:", err)
		os.Exit(1)
	}
	out, _ := json.Marshal(s)
	fmt.Println(string(out))
}
