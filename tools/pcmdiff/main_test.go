package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func encodePCM(vals []float64, width int) []byte {
	b := make([]byte, len(vals)*width)
	for i, v := range vals {
		if width == 8 {
			binary.LittleEndian.PutUint64(b[i*width:], math.Float64bits(v))
		} else {
			binary.LittleEndian.PutUint32(b[i*width:], math.Float32bits(float32(v)))
		}
	}
	return b
}

func compare(ref, out []float64, channels int) (*stats, error) {
	return compareReaders(bytes.NewReader(encodePCM(ref, 8)), bytes.NewReader(encodePCM(out, 8)), 8, 8, channels, -1)
}

func TestCompareKnownError(t *testing.T) {
	ref := []float64{0.1, -0.1, 0.2, -0.2}
	out := []float64{0.101, -0.1, 0.201, -0.2}
	s, err := compare(ref, out, 2)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(s.rmsErr[0]-0.001) > 1e-12 || s.rmsErr[1] != 0 {
		t.Fatalf("rmsErr=%v, want [0.001 0]", s.rmsErr)
	}
	if math.Abs(s.maxErr[0]-0.001) > 1e-12 || s.maxErrAt[0] != 0 {
		t.Fatalf("maxErr[0]=%g at %d, want 0.001 at 0", s.maxErr[0], s.maxErrAt[0])
	}
	if math.Abs(s.peakOut[0]-0.201) > 1e-12 {
		t.Fatalf("peakOut[0]=%g, want 0.201", s.peakOut[0])
	}
	// 第 1 声道完全相同。帧数与参考峰值按采样帧、按声道统计
	if s.framesRef != 2 || s.framesOut != 2 || s.maxErr[1] != 0 {
		t.Fatalf("frames ref=%d out=%d, maxErr[1]=%g, want 2, 2, 0", s.framesRef, s.framesOut, s.maxErr[1])
	}
	if s.peakRef[0] != 0.2 || s.peakRef[1] != 0.2 || s.peakOut[1] != 0.2 {
		t.Fatalf("peaks ref=%v out=%v, want 0.2 on channel 1 and ref channel 0", s.peakRef, s.peakOut)
	}
}

func TestViolationsPeakDB(t *testing.T) {
	cases := []struct {
		name            string
		ref, out        []float64
		channels        int
		rms, maxErr, db float64
		wantN           int
		wantLast        []string // 最后一条违规须包含的片段
	}{
		{"digital silence", make([]float64, 8), make([]float64, 8), 2, 1e-5, 1e-3, 0.1, 0, nil},
		// 放宽 RMS 与最大误差门槛,只留峰值判定
		{"peak 0.2 dB high", []float64{0.5, 0}, []float64{0.5 * math.Pow(10, 0.2/20), 0}, 1, 1, 1, 0.1, 1, []string{"peak"}},
		// 参考峰值 -80 dBFS,低于 -60 dBFS。放宽 RMS 与 dB 门槛,1e-3 的绝对误差门槛须触发
		{"quiet reference", []float64{1e-4, 0}, []float64{1e-4 + 2e-3, 0}, 1, 1, 1e-3, 100, 2, []string{"peak", "below -60 dBFS"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := compare(tc.ref, tc.out, tc.channels)
			if err != nil {
				t.Fatal(err)
			}
			v := s.violations(tc.rms, tc.maxErr, tc.db)
			if len(v) != tc.wantN {
				t.Fatalf("violations=%v, want %d", v, tc.wantN)
			}
			for _, sub := range tc.wantLast {
				if !strings.Contains(v[len(v)-1], sub) {
					t.Fatalf("violations=%v, want last to contain %q", v, sub)
				}
			}
		})
	}
}

func TestSkipFrames(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6}
	r := bytes.NewReader(encodePCM(s, 8))
	if err := skipPCM(r, 8, 2, 1); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, encodePCM([]float64{3, 4, 5, 6}, 8)) {
		t.Fatalf("got %x, want encoded [3 4 5 6]", got)
	}
	if err := skipPCM(bytes.NewReader(encodePCM(s, 8)), 8, 2, 4); err == nil {
		t.Fatal("want error for skip beyond end")
	}
	if err := skipPCM(bytes.NewReader(encodePCM(s, 8)), 8, 2, -1); err == nil {
		t.Fatal("want error for negative skip")
	}
}

func TestCompareAcrossBlocksAndShortReads(t *testing.T) {
	const channels = 3
	ref := make([]float64, channels*(pcmBlockSamples+1))
	for i := range ref {
		ref[i] = float64(i%17) / 32
	}
	out := append([]float64(nil), ref...)
	const changed = pcmBlockSamples + 2
	out[changed] += 0.125
	s, err := compareReaders(
		iotest.OneByteReader(bytes.NewReader(encodePCM(ref, 8))),
		iotest.HalfReader(bytes.NewReader(encodePCM(out, 4))), 8, 4, channels, -1)
	if err != nil {
		t.Fatal(err)
	}
	c := changed % channels
	if s.framesRef != pcmBlockSamples+1 || s.maxErr[c] != 0.125 || s.maxErrAt[c] != changed/channels {
		t.Fatalf("block boundary changed channel/frame: %+v", s)
	}
	wantRMS := 0.125 / math.Sqrt(pcmBlockSamples+1)
	if math.Abs(s.rmsErr[c]-wantRMS) > 1e-12 {
		t.Fatalf("RMS %g, want %g", s.rmsErr[c], wantRMS)
	}
	for i := range channels {
		if i != c && (s.rmsErr[i] != 0 || s.maxErr[i] != 0) {
			t.Fatalf("unexpected error in channel %d", i)
		}
	}
}

func TestCompareStreamLimitsAndMalformedInput(t *testing.T) {
	good := encodePCM([]float64{0.5, -0.5, math.NaN()}, 8)
	ref, out := bytes.NewReader(good), bytes.NewReader(good)
	if _, err := compareReaders(ref, out, 8, 8, 2, 1); err != nil {
		t.Fatalf("unused suffix must not be compared: %v", err)
	}
	if ref.Len() != 8 || out.Len() != 8 {
		t.Fatal("-frames read beyond the requested prefix")
	}
	cases := []struct {
		name     string
		ref, out []byte
		channels int
		frames   int
	}{
		{"partial sample", []byte{1}, []byte{1}, 1, -1},
		{"partial channel frame", good[:8], good[:8], 2, -1},
		{"length mismatch", good[:16], good[:8], 1, -1},
		{"short requested prefix", good[:16], good[:16], 2, 2},
		{"zero channels", nil, nil, 0, -1},
		{"NaN in ref", encodePCM([]float64{math.NaN()}, 8), encodePCM([]float64{0}, 8), 1, -1},
		{"Inf in out", encodePCM([]float64{0}, 8), encodePCM([]float64{math.Inf(1)}, 8), 1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := compareReaders(bytes.NewReader(tc.ref), bytes.NewReader(tc.out), 8, 8, tc.channels, tc.frames); err == nil {
				t.Fatal("want malformed input error")
			}
		})
	}
}

func TestRunSkipsNaNAndValidatesUnusedFileTail(t *testing.T) {
	dir := t.TempDir()
	ref, out := filepath.Join(dir, "ref.f64le"), filepath.Join(dir, "out.f32le")
	if err := os.WriteFile(ref, encodePCM([]float64{math.NaN(), 0.5, -0.5}, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, encodePCM([]float64{0.5, -0.5, math.NaN()}, 4), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := run(ref, out, "f64le", "f32le", 1, 1, 0, 2, 1e-5, 1e-3, 0.1, io.Discard)
	if err != nil || len(v) != 0 {
		t.Fatalf("documented skips and prefix: violations=%v, err=%v", v, err)
	}
	if _, err := run(ref, out, "f64le", "f32le", 1, 4, 0, 0, 1e-5, 1e-3, 0.1, io.Discard); err == nil {
		t.Fatal("regular-file seek beyond EOF must fail")
	}
	if err := os.WriteFile(out, append(encodePCM([]float64{0.5, -0.5}, 4), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ref, out, "f64le", "f32le", 1, 1, 0, 1, 1e-5, 1e-3, 0.1, io.Discard); err == nil {
		t.Fatal("partial PCM sample must fail even outside -frames")
	}
}
