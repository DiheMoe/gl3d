// Command gl3ddec 用 gl3d 解码 MP3 文件,把交错的 float64 小端 PCM 写入 OUT,并以
// JSON 输出格式与采样帧计数。PCM 布局与 refgen 写出的 FFmpeg 参考相同,供 pcmdiff
// 比较。
//
// 用法:gl3ddec INPUT.mp3 OUT.f64le
package main

import (
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
	srcInfo, err := src.Stat()
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	d, err := gl3d.NewDecoder(ctx, src)
	if err != nil {
		return nil, err
	}
	// Inspect the actual opened file before truncating it, so aliases and a path
	// changed between open and stat cannot destroy the input.
	dst, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	defer dst.Close()
	dstInfo, err := dst.Stat()
	if err != nil {
		return nil, err
	}
	if os.SameFile(srcInfo, dstInfo) {
		return nil, fmt.Errorf("input and output refer to the same file")
	}
	if dstInfo.Mode().IsRegular() {
		if err := dst.Truncate(0); err != nil {
			return nil, err
		}
	}
	if err := writePCM(ctx, d, dst); err != nil {
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

// Consume the borrowed PCM before requesting another chunk. Serialize into one
// reusable byte block: no float64 copy and no per-sample Writer calls.
func writePCM(ctx context.Context, d *gl3d.Decoder, w io.Writer) error {
	var encoded [32 << 10]byte
	for {
		pcm, err := d.NextPCM(ctx)
		for len(pcm) > 0 {
			n := min(len(pcm), len(encoded)/8)
			b := encoded[:n*8]
			for i, v := range pcm[:n] {
				binary.LittleEndian.PutUint64(b[i*8:], math.Float64bits(v))
			}
			written, werr := w.Write(b)
			if werr != nil {
				return werr
			}
			if written != len(b) {
				return io.ErrShortWrite
			}
			pcm = pcm[n:]
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
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
