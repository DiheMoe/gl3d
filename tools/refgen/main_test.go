package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWritesReferences(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "mpg123"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool, "not installed")
		}
	}
	in := "../../corpus/synthetic/short-tone-stereo-44k1-cbr192.mp3"
	data, err := os.ReadFile(in)
	if err != nil {
		t.Skip("corpus file not available:", err)
	}
	dir := t.TempDir()
	if err := run([]string{in}, dir); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "short-tone-stereo-44k1-cbr192")
	// FFmpeg 按 LAME 标签裁剪为 3 帧 × 1152 − 576 − 675,mpg123 不裁剪
	for ext, size := range map[string]int64{".ffmpeg.f64le": 2205 * 2 * 8, ".mpg123.f32le": 3456 * 2 * 4} {
		fi, err := os.Stat(base + ext)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != size {
			t.Errorf("%s: %d bytes, want %d", ext, fi.Size(), size)
		}
	}
	meta, err := os.ReadFile(base + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	for _, want := range []string{
		"\ninput_sha256=" + hex.EncodeToString(sum[:]) + "\n",
		"\nffmpeg=ffmpeg version ",
		"\nmpg123=mpg123 ",
		"\nffmpeg_cmd=ffmpeg -v error -nostdin -i INPUT -map 0:a:0 -f f64le -acodec pcm_f64le OUT\n",
		"\nmpg123_cmd=mpg123 -q --no-gapless --float -s INPUT\n",
	} {
		if !strings.Contains(string(meta), want) {
			t.Errorf("meta lacks %q:\n%s", want, meta)
		}
	}
}
