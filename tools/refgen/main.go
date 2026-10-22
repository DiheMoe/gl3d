// Command refgen 用参考解码器解码 MP3 文件,为每个输入在 OUTDIR 写出 NAME.ffmpeg.f64le
// (判定基准)、NAME.mpg123.f32le(交叉检查)和记录输入哈希、工具版本与命令的
// NAME.meta。两个解码器都不做增益控制、重采样或声道转换;FFmpeg 默认按 LAME 标签做
// gapless 裁剪,mpg123 以 --no-gapless 输出全部帧的未裁剪解码。参考工具版本冻结在
// corpus/manifest.json 的 referenceTools,由 corpuscheck 核对。
//
// 端到端测试从 corpus/synthetic/ref 读取参考输出,在仓库根目录生成:
//
//	go -C tools run ./refgen "$PWD"/corpus/synthetic/*.mp3 "$PWD"/corpus/synthetic/ref
//
// 用法:refgen INPUT.mp3... OUTDIR
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 参考解码命令的参数,meta 原样记录;INPUT 与 OUT 在运行时换成实际路径。
var (
	ffmpegArgs = []string{"-v", "error", "-nostdin", "-i", "INPUT", "-map", "0:a:0", "-f", "f64le", "-acodec", "pcm_f64le", "OUT"}
	mpg123Args = []string{"-q", "--no-gapless", "--float", "-s", "INPUT"}
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: refgen INPUT.mp3... OUTDIR")
		os.Exit(2)
	}
	if err := run(os.Args[1:len(os.Args)-1], os.Args[len(os.Args)-1]); err != nil {
		fmt.Fprintln(os.Stderr, "refgen:", err)
		os.Exit(1)
	}
}

func run(inputs []string, outdir string) error {
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return err
	}
	ffmpegVersion, err := firstLine("ffmpeg", "-version")
	if err != nil {
		return err
	}
	mpg123Version, err := firstLine("mpg123", "--version")
	if err != nil {
		return err
	}
	for _, in := range inputs {
		base := filepath.Join(outdir, strings.TrimSuffix(filepath.Base(in), filepath.Ext(in)))
		// -y 只允许覆盖已有输出,不影响解码结果,meta 不记录
		ff := exec.Command("ffmpeg", append([]string{"-y"}, subst(ffmpegArgs, in, base+".ffmpeg.f64le")...)...)
		ff.Stderr = os.Stderr
		if err := ff.Run(); err != nil {
			return fmt.Errorf("ffmpeg %s: %w", in, err)
		}
		pcm, err := os.Create(base + ".mpg123.f32le")
		if err != nil {
			return err
		}
		mp := exec.Command("mpg123", subst(mpg123Args, in, "")...)
		mp.Stdout, mp.Stderr = pcm, os.Stderr
		err = mp.Run()
		if cerr := pcm.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("mpg123 %s: %w", in, err)
		}
		data, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		meta := fmt.Sprintf("input=%s\ninput_sha256=%s\nffmpeg=%s\nmpg123=%s\nffmpeg_cmd=ffmpeg %s\nmpg123_cmd=mpg123 %s\n",
			in, hex.EncodeToString(sum[:]), ffmpegVersion, mpg123Version,
			strings.Join(ffmpegArgs, " "), strings.Join(mpg123Args, " "))
		if err := os.WriteFile(base+".meta", []byte(meta), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func subst(args []string, in, out string) []string {
	r := make([]string, len(args))
	for i, a := range args {
		switch a {
		case "INPUT":
			a = in
		case "OUT":
			a = out
		}
		r[i] = a
	}
	return r
}

// firstLine 返回命令标准输出的第一行。
func firstLine(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	line, _, _ := bytes.Cut(out, []byte("\n"))
	return string(line), nil
}
