package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunWritesF64LE(t *testing.T) {
	in := "../../corpus/synthetic/short-tone-stereo-44k1-cbr192.mp3"
	if _, err := os.Stat(in); err != nil {
		t.Skip("corpus file not available:", err)
	}
	out := filepath.Join(t.TempDir(), "tone.f64le")
	s, err := run(in, out)
	if err != nil {
		t.Fatal(err)
	}
	// 3 帧 × 1152 − enc_delay 576 − enc_padding 675 = 2205
	if s.Frames != 2205 || s.DecodedFrames != 3456 || !s.Trimmed || !s.Clean || s.Channels != 2 || s.SampleRate != 44100 {
		t.Fatalf("summary %+v", s)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 2205*2*8 {
		t.Fatalf("output %d bytes, want %d", fi.Size(), 2205*2*8)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if clean, ok := fields["clean"].(bool); !ok || !clean {
		t.Fatalf(`JSON "clean" = %v, want true: %s`, fields["clean"], b)
	}
}

// 默认模式是宽松的:音频之后的垃圾不使解码失败,只让 clean 为假。
func TestRunLenientReportsNotClean(t *testing.T) {
	data, err := os.ReadFile("../../corpus/synthetic/short-tone-stereo-44k1-cbr192.mp3")
	if err != nil {
		t.Skip("corpus file not available:", err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "garbage-tail.mp3")
	if err := os.WriteFile(in, append(data, make([]byte, 300)...), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := run(in, filepath.Join(dir, "out.f64le"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Clean || s.Frames != 2205 {
		t.Fatalf("summary %+v, want Clean=false and 2205 frames", s)
	}
}

func TestRunReportsDecodeErrors(t *testing.T) {
	in := filepath.Join(t.TempDir(), "empty.mp3")
	if err := os.WriteFile(in, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(in, filepath.Join(t.TempDir(), "out.f64le")); err == nil {
		t.Fatal("want an error for empty input")
	}
}
