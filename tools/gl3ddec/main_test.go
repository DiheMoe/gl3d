package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gl3d "github.com/DiheMoe/gl3d"
)

func TestRunWritesF64LE(t *testing.T) {
	in := "../../corpus/synthetic/short-tone-stereo-44k1-cbr192.mp3"
	if _, err := os.Stat(in); err != nil {
		t.Skip("corpus file not available:", err)
	}
	out := filepath.Join(t.TempDir(), "tone.f64le")
	if err := os.WriteFile(out, make([]byte, 100000), 0o644); err != nil {
		t.Fatal(err)
	}
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
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	d, err := gl3d.NewDecoder(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	var pcm [4096]float64
	for {
		n, err := d.ReadPCM(context.Background(), pcm[:])
		for _, v := range pcm[:n*d.Channels()] {
			want = binary.LittleEndian.AppendUint64(want, math.Float64bits(v))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("exported bytes differ from ReadPCM f64le output")
	}
}

func TestRunReportsDecodeErrors(t *testing.T) {
	in := filepath.Join(t.TempDir(), "empty.mp3")
	if err := os.WriteFile(in, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.f64le")
	if err := os.WriteFile(out, []byte("keep existing output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(in, out); err == nil {
		t.Fatal("want an error for empty input")
	}
	got, err := os.ReadFile(out)
	if err != nil || string(got) != "keep existing output" {
		t.Fatalf("invalid input changed existing output: %q, %v", got, err)
	}
}

func TestRunRejectsSameFileAndAliases(t *testing.T) {
	data, err := os.ReadFile("../../corpus/synthetic/tone10ms-stereo-44k1-cbr128-lame.mp3")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"same path", "hard link", "symbolic link"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			in := filepath.Join(dir, "input.mp3")
			if err := os.WriteFile(in, data, 0o644); err != nil {
				t.Fatal(err)
			}
			out := in
			if kind != "same path" {
				out = filepath.Join(dir, "alias.f64le")
				link := os.Link
				if kind == "symbolic link" {
					link = os.Symlink
				}
				if err := link(in, out); err != nil {
					t.Skipf("%s unavailable: %v", kind, err)
				}
			}
			if _, err := run(in, out); err == nil || !strings.Contains(err.Error(), "same file") {
				t.Fatalf("alias err=%v, want same file error", err)
			}
			got, err := os.ReadFile(in)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("input was modified: %d bytes, %v", len(got), err)
			}
		})
	}
}

type failedWriter struct {
	err   error
	calls int
}

func (w *failedWriter) Write(p []byte) (int, error) {
	w.calls++
	return len(p) - 1, w.err
}

func TestWritePCMStopsOnOutputFailure(t *testing.T) {
	data, err := os.ReadFile("../../corpus/synthetic/sine440-stereo-44k1-cbr192.mp3")
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("output failed")
	for _, werr := range []error{failure, nil} {
		d, err := gl3d.NewDecoder(context.Background(), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		w := &failedWriter{err: werr}
		err = writePCM(context.Background(), d, w)
		want := werr
		if want == nil {
			want = io.ErrShortWrite
		}
		if !errors.Is(err, want) || w.calls != 1 {
			t.Fatalf("error=%v, writes=%d; want %v after one write", err, w.calls, want)
		}
		if d.DecodedFrames() >= 193*1152 {
			t.Fatal("decoded the full input after output failure")
		}
	}
}
