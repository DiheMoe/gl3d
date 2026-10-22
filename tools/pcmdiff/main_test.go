package main

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareIdentical(t *testing.T) {
	ref := []float64{0.1, -0.1, 0.2, -0.2}
	s, err := compare(ref, append([]float64(nil), ref...), 2)
	if err != nil {
		t.Fatal(err)
	}
	if s.framesRef != 2 || s.framesOut != 2 {
		t.Fatalf("frames ref=%d out=%d, want 2", s.framesRef, s.framesOut)
	}
	for c := 0; c < 2; c++ {
		if s.rmsErr[c] != 0 || s.maxErr[c] != 0 {
			t.Fatalf("channel %d: rms=%g max=%g, want 0", c, s.rmsErr[c], s.maxErr[c])
		}
		if s.peakRef[c] != 0.2 || s.peakOut[c] != 0.2 {
			t.Fatalf("channel %d: peaks ref=%g out=%g, want 0.2", c, s.peakRef[c], s.peakOut[c])
		}
	}
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
}

func TestCompareRejectsLengthMismatch(t *testing.T) {
	if _, err := compare([]float64{0, 0}, []float64{0}, 2); err == nil {
		t.Fatal("want error for length mismatch")
	}
}

func TestCompareRejectsNaNAndInf(t *testing.T) {
	if _, err := compare([]float64{math.NaN()}, []float64{0}, 1); err == nil {
		t.Fatal("want error for NaN")
	}
	if _, err := compare([]float64{0}, []float64{math.Inf(1)}, 1); err == nil {
		t.Fatal("want error for Inf")
	}
}

func TestViolationsSilencePasses(t *testing.T) {
	s, err := compare(make([]float64, 8), make([]float64, 8), 2)
	if err != nil {
		t.Fatal(err)
	}
	if v := s.violations(1e-5, 1e-3, 0.1); len(v) != 0 {
		t.Fatalf("digital silence must not violate: %v", v)
	}
}

func TestViolationsPeakDB(t *testing.T) {
	ref := []float64{0.5, 0}
	out := []float64{0.5 * math.Pow(10, 0.2/20), 0} // 峰值高 0.2 dB
	s, err := compare(ref, out, 1)
	if err != nil {
		t.Fatal(err)
	}
	v := s.violations(1, 1, 0.1) // 放宽 RMS 与最大误差门槛,只留峰值判定
	if len(v) != 1 || !strings.Contains(v[0], "peak") {
		t.Fatalf("violations=%v, want one peak violation", v)
	}
}

func TestViolationsQuietReferenceUsesAbsolutePeakLimit(t *testing.T) {
	ref := []float64{1e-4, 0} // 峰值 -80 dBFS,低于 -60 dBFS
	out := []float64{1e-4 + 2e-3, 0}
	s, err := compare(ref, out, 1)
	if err != nil {
		t.Fatal(err)
	}
	v := s.violations(1, 1e-3, 100) // 放宽 RMS 与 dB 门槛,1e-3 的绝对误差门槛须触发
	if len(v) != 2 {
		t.Fatalf("violations=%v, want max + absolute peak violations", v)
	}
	if !strings.Contains(v[1], "peak") || !strings.Contains(v[1], "below -60 dBFS") {
		t.Fatalf("violations=%v, want absolute peak violation", v)
	}
}

func TestLoadPCMFormats(t *testing.T) {
	dir := t.TempDir()

	vals := []float64{0.5, -0.25, 1.5}
	buf64 := make([]byte, 8*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint64(buf64[i*8:], math.Float64bits(v))
	}
	p64 := filepath.Join(dir, "a.f64le")
	if err := os.WriteFile(p64, buf64, 0o666); err != nil {
		t.Fatal(err)
	}
	got, err := loadPCM(p64, "f64le")
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vals {
		if got[i] != v {
			t.Fatalf("f64le sample %d = %g, want %g", i, got[i], v)
		}
	}

	vals32 := []float32{0.5, -0.25, 1.5}
	buf32 := make([]byte, 4*len(vals32))
	for i, v := range vals32 {
		binary.LittleEndian.PutUint32(buf32[i*4:], math.Float32bits(v))
	}
	p32 := filepath.Join(dir, "a.f32le")
	if err := os.WriteFile(p32, buf32, 0o666); err != nil {
		t.Fatal(err)
	}
	got, err = loadPCM(p32, "f32le")
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vals32 {
		if got[i] != float64(v) {
			t.Fatalf("f32le sample %d = %g, want %g", i, got[i], float64(v))
		}
	}
}

func TestLoadPCMRejectsTruncatedSample(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.f64le")
	if err := os.WriteFile(p, []byte{1, 2, 3}, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPCM(p, "f64le"); err == nil {
		t.Fatal("want error for 3-byte f64le file")
	}
}

func TestSkipFrames(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6}
	got, err := skipFrames(s, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != 3 {
		t.Fatalf("got %v, want [3 4 5 6]", got)
	}
	if _, err := skipFrames(s, 2, 4); err == nil {
		t.Fatal("want error for skip beyond end")
	}
	if _, err := skipFrames(s, 2, -1); err == nil {
		t.Fatal("want error for negative skip")
	}
}

func TestRunWithFramesLimitsComparison(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, vals []float64) string {
		buf := make([]byte, 8*len(vals))
		for i, v := range vals {
			binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, buf, 0o666); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ref := write("ref.f64le", []float64{1, 2, 3, 4})
	out := write("out.f64le", []float64{1, 2, 3, 4, 5, 6})
	var sb strings.Builder
	if _, err := run(ref, out, "f64le", "f64le", 1, 0, 0, 4, 1e-5, 1e-3, 0.1, &sb); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ref, out, "f64le", "f64le", 1, 0, 0, 5, 1e-5, 1e-3, 0.1, &sb); err == nil {
		t.Fatal("want error when -frames exceeds available ref frames")
	}
	if _, err := run(ref, out, "f64le", "f64le", 1, 0, 0, -1, 1e-5, 1e-3, 0.1, &sb); err == nil {
		t.Fatal("want count mismatch error without -frames")
	}
}
