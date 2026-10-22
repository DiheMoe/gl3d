package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture 在临时目录中放置 short-tone 样例(3 个音频帧,Info+LAME 576/675,
// joint stereo 192 kbps)及长度自洽的参考输出,返回根目录与清单条目。
func fixture(t *testing.T) (string, manifest, entry) {
	t.Helper()
	data, err := os.ReadFile("../../corpus/synthetic/short-tone-stereo-44k1-cbr192.mp3")
	if err != nil {
		t.Skip("corpus file not available:", err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "corpus", "synthetic")
	if err := os.MkdirAll(filepath.Join(dir, "ref"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(data)
	write("tone.mp3", data)
	write("ref/tone.ffmpeg.f64le", make([]byte, 2205*2*8)) // 3×1152 − 576 − 675
	write("ref/tone.mpg123.f32le", make([]byte, 3456*2*4))
	write("ref/tone.meta", []byte("input_sha256="+hex.EncodeToString(sum[:])+"\nffmpeg=ffmpeg version n9.0.2\nmpg123=mpg123 1.33.7\n"))

	var e entry
	e.File = "corpus/synthetic/tone.mp3"
	e.SHA256 = hex.EncodeToString(sum[:])
	e.ExpectedFormat.MPEGVersion = "1"
	e.ExpectedFormat.SampleRate = 44100
	e.ExpectedFormat.Channels = 2
	e.ExpectedFormat.ChannelMode = "joint_stereo"
	e.ExpectedFormat.BitrateMode = "CBR"
	kbps, delay, padding, frames := 192, 576, 675, int64(2205)
	e.ExpectedFormat.BitrateKbps = &kbps
	e.Trim.Kind = "lame-gapless"
	e.Trim.EncDelay, e.Trim.EncPadding = &delay, &padding
	e.ExpectedFrames = &frames
	return root, manifest{}, e
}

func TestCheckEntryConsistent(t *testing.T) {
	root, m, e := fixture(t)
	if p := checkEntry(root, &m, &e); len(p) != 0 {
		t.Fatalf("unexpected problems: %v", p)
	}
}

func TestCheckEntryDetectsInconsistencies(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(root string, m *manifest, e *entry)
		want   string
	}{
		{"hash", func(_ string, _ *manifest, e *entry) { e.SHA256 = strings.Repeat("0", 64) }, "sha256"},
		{"channel mode from the Info frame", func(_ string, _ *manifest, e *entry) { e.ExpectedFormat.ChannelMode = "stereo" }, "first audio frame"},
		{"bitrate", func(_ string, _ *manifest, e *entry) { v := 128; e.ExpectedFormat.BitrateKbps = &v }, "CBR"},
		{"trim", func(_ string, _ *manifest, e *entry) { v := 1000; e.Trim.EncPadding = &v }, "LAME tag"},
		{"expected frames", func(_ string, _ *manifest, e *entry) { v := int64(2206); e.ExpectedFrames = &v }, "expectedFrames"},
		{"untrimmed reference length", func(root string, _ *manifest, _ *entry) {
			os.WriteFile(filepath.Join(root, "corpus/synthetic/ref/tone.mpg123.f32le"), make([]byte, 2304*2*4), 0o644)
		}, "mpg123"},
		{"frozen tool version", func(_ string, m *manifest, _ *entry) {
			m.ReferenceTools = &struct {
				FFmpeg string `json:"ffmpeg"`
				MPG123 string `json:"mpg123"`
			}{"ffmpeg version n8.1", "mpg123 1.33.7"}
		}, "references made with"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, m, e := fixture(t)
			tc.mutate(root, &m, &e)
			p := checkEntry(root, &m, &e)
			if !strings.Contains(strings.Join(p, "\n"), tc.want) {
				t.Fatalf("problems %v, want one mentioning %q", p, tc.want)
			}
		})
	}
}
