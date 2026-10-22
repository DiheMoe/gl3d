// Command corpuscheck 用语料文件、独立于 gl3d 的帧头解析器(internal/mp3info)和
// refgen 写出的参考输出核对 corpus/manifest.json 的每个条目:
//
//   - 文件哈希;
//   - expectedFormat 与第一个音频帧一致,bitrateMode/bitrateKbps 与全部帧头及
//     Xing/LAME 标签一致;
//   - trim 与 LAME 标签一致,Xing 声明的帧数与逐帧扫描一致;
//   - 参考输出的输入哈希与工具版本与清单一致,mpg123 输出是全部帧的未裁剪解码,
//     FFmpeg 输出等于 expectedFrames,两者之差恰为声明的裁剪量。
//
// 条目标为 frozen 前须通过本检查。退出码:0 全部一致,1 有条目不一致,2 清单无法读取。
//
// 用法(在仓库根目录):go -C tools run ./corpuscheck [-root ..] [-manifest corpus/manifest.json]
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DiheMoe/gl3d/tools/internal/mp3info"
)

type manifest struct {
	ReferenceTools *struct {
		FFmpeg string `json:"ffmpeg"`
		MPG123 string `json:"mpg123"`
	} `json:"referenceTools"`
	Entries []entry `json:"entries"`
}

type entry struct {
	File           string `json:"file"`
	SHA256         string `json:"sha256"`
	ExpectedFormat struct {
		MPEGVersion string `json:"mpegVersion"`
		SampleRate  int    `json:"sampleRate"`
		Channels    int    `json:"channels"`
		ChannelMode string `json:"channelMode"`
		BitrateMode string `json:"bitrateMode"`
		BitrateKbps *int   `json:"bitrateKbps"`
	} `json:"expectedFormat"`
	Trim struct {
		Kind       string `json:"kind"`
		EncDelay   *int   `json:"encDelay"`
		EncPadding *int   `json:"encPadding"`
	} `json:"trim"`
	ExpectedFrames *int64 `json:"expectedFrames"`
	Status         string `json:"status"`
}

func main() {
	root := flag.String("root", "..", "repository root")
	path := flag.String("manifest", "corpus/manifest.json", "manifest path relative to root")
	flag.Parse()
	data, err := os.ReadFile(filepath.Join(*root, *path))
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpuscheck:", err)
		os.Exit(2)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		fmt.Fprintln(os.Stderr, "corpuscheck:", err)
		os.Exit(2)
	}
	failed := 0
	for _, e := range m.Entries {
		problems := checkEntry(*root, &m, &e)
		if len(problems) == 0 {
			fmt.Printf("ok    %s\n", e.File)
			continue
		}
		failed++
		for _, p := range problems {
			fmt.Printf("FAIL  %s: %s\n", e.File, p)
		}
	}
	fmt.Printf("%d entries, %d failed\n", len(m.Entries), failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func checkEntry(root string, m *manifest, e *entry) []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	data, err := os.ReadFile(filepath.Join(root, e.File))
	if err != nil {
		return []string{err.Error()}
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != e.SHA256 {
		fail("sha256 %x, manifest %s", sum, e.SHA256)
	}
	info, err := mp3info.Parse(data)
	if err != nil {
		return append(problems, "parse: "+err.Error())
	}
	if info.Audio == nil {
		return append(problems, "no audio frame after the tag frame")
	}
	ef, a := e.ExpectedFormat, info.Audio
	if ef.MPEGVersion != a.MPEGVersion || ef.SampleRate != a.SampleRate || ef.Channels != a.Channels || ef.ChannelMode != a.ChannelMode {
		fail("format %s/%d Hz/%d ch/%s, first audio frame %s/%d Hz/%d ch/%s",
			ef.MPEGVersion, ef.SampleRate, ef.Channels, ef.ChannelMode, a.MPEGVersion, a.SampleRate, a.Channels, a.ChannelMode)
	}

	// 流须恰好结束于帧边界或 ID3v1 标签之前
	off := 0
	if info.ID3v2 != nil {
		off = info.ID3v2.Size
	}
	if info.Xing != nil || info.VBRI != nil {
		off += info.FrameSize
	}
	end := len(data)
	if info.ID3v1 {
		end -= 128
	}
	frames := 0
	bitrates := map[int]bool{}
	for off < end {
		h, err := mp3info.ParseHeader(data[:end], off)
		if err != nil {
			fail("frame %d at byte %d: %v", frames, off, err)
			break
		}
		if off+h.FrameSize > end {
			fail("frame %d at byte %d is truncated", frames, off)
			break
		}
		bitrates[h.BitrateKbps] = true
		frames++
		off += h.FrameSize
	}
	switch ef.BitrateMode {
	case "CBR":
		if len(bitrates) != 1 || ef.BitrateKbps == nil || !bitrates[*ef.BitrateKbps] {
			fail("CBR %v kbps, frames use %v", ef.BitrateKbps, keys(bitrates))
		}
		if info.Xing != nil && info.Xing.Kind != "info" {
			fail("CBR stream with a %q tag", info.Xing.Kind)
		}
	case "VBR", "ABR":
		if ef.BitrateKbps != nil {
			fail("%s entry with bitrateKbps %d", ef.BitrateMode, *ef.BitrateKbps)
		}
		if info.Xing == nil || info.Xing.Kind != "xing" {
			fail("%s stream without a Xing tag", ef.BitrateMode)
		}
	default:
		fail("unknown bitrateMode %q", ef.BitrateMode)
	}
	if info.LAME != nil {
		want := map[int]string{1: "CBR", 2: "ABR", 3: "VBR", 4: "VBR", 5: "VBR", 6: "VBR", 8: "CBR", 9: "ABR"}[info.LAME.VBRMethod]
		if want != "" && want != ef.BitrateMode {
			fail("LAME VBR method %d means %s, manifest %s", info.LAME.VBRMethod, want, ef.BitrateMode)
		}
	}
	if info.Xing != nil && info.Xing.Frames != nil && int(*info.Xing.Frames) != frames {
		fail("Xing declares %d frames, walk found %d", *info.Xing.Frames, frames)
	}

	switch e.Trim.Kind {
	case "lame-gapless":
		if info.LAME == nil {
			fail("trim lame-gapless without a LAME tag")
		} else if e.Trim.EncDelay == nil || e.Trim.EncPadding == nil ||
			*e.Trim.EncDelay != info.LAME.EncDelay || *e.Trim.EncPadding != info.LAME.EncPadding {
			fail("trim %v/%v, LAME tag %d/%d", e.Trim.EncDelay, e.Trim.EncPadding, info.LAME.EncDelay, info.LAME.EncPadding)
		}
	case "none":
		if info.LAME != nil {
			fail("trim none but the stream has a LAME tag")
		}
	default:
		fail("unknown trim kind %q", e.Trim.Kind)
	}

	base := strings.TrimSuffix(filepath.Base(e.File), filepath.Ext(e.File))
	refDir := filepath.Join(root, filepath.Dir(e.File), "ref")
	meta, err := readMeta(filepath.Join(refDir, base+".meta"))
	if err != nil {
		return append(problems, "reference outputs missing (run tools/refgen): "+err.Error())
	}
	if meta["input_sha256"] != e.SHA256 {
		fail("references were generated from input %s", meta["input_sha256"])
	}
	if rt := m.ReferenceTools; rt != nil && (meta["ffmpeg"] != rt.FFmpeg || meta["mpg123"] != rt.MPG123) {
		fail("references made with %q / %q, manifest freezes %q / %q", meta["ffmpeg"], meta["mpg123"], rt.FFmpeg, rt.MPG123)
	}
	ff, err1 := os.Stat(filepath.Join(refDir, base+".ffmpeg.f64le"))
	mp, err2 := os.Stat(filepath.Join(refDir, base+".mpg123.f32le"))
	if err1 != nil || err2 != nil {
		return append(problems, "reference PCM missing (run tools/refgen)")
	}
	spf := int64(1152)
	if a.MPEGVersion != "1" {
		spf = 576
	}
	untrimmed := int64(frames) * spf
	ffFrames := ff.Size() / 8 / int64(a.Channels)
	mpFrames := mp.Size() / 4 / int64(a.Channels)
	if mpFrames != untrimmed {
		fail("mpg123 --no-gapless gives %d frames, %d audio frames × %d = %d", mpFrames, frames, spf, untrimmed)
	}
	trimmed := untrimmed
	if e.Trim.Kind == "lame-gapless" && e.Trim.EncDelay != nil && e.Trim.EncPadding != nil {
		trimmed -= int64(*e.Trim.EncDelay + *e.Trim.EncPadding)
	}
	if ffFrames != trimmed {
		fail("FFmpeg gives %d frames, untrimmed %d minus declared trim = %d", ffFrames, untrimmed, trimmed)
	}
	if e.ExpectedFrames == nil || *e.ExpectedFrames != ffFrames {
		fail("expectedFrames %v, FFmpeg reference %d", e.ExpectedFrames, ffFrames)
	}
	return problems
}

func keys(m map[int]bool) []int {
	var out []int
	for k := range m {
		out = append(out, k)
	}
	return out
}

// readMeta 读取 refgen 写出的 key=value 元数据。
func readMeta(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	meta := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			meta[k] = v
		}
	}
	return meta, sc.Err()
}
