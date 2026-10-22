package main

import (
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
	if s.Frames != 2205 || s.DecodedFrames != 3456 || !s.Trimmed || s.Channels != 2 || s.SampleRate != 44100 {
		t.Fatalf("summary %+v", s)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 2205*2*8 {
		t.Fatalf("output %d bytes, want %d", fi.Size(), 2205*2*8)
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
