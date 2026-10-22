// Command pcmdiff 逐样本比较两个小端 PCM 文件,按声道报告误差统计并对照门槛判定。
// 退出码:0 在门槛内,1 超出门槛,2 用法或数据错误。
//
// 对齐只按 -skip-ref/-skip-out 给出的帧数进行,这些偏移须能由解码延迟或 gapless
// 规则解释;工具不搜索最优偏移,也不拟合增益。
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
)

type stats struct {
	channels  int
	framesRef int
	framesOut int
	rmsErr    []float64
	maxErr    []float64
	maxErrAt  []int
	peakRef   []float64
	peakOut   []float64
}

func compare(ref, out []float64, channels int) (*stats, error) {
	if channels < 1 {
		return nil, fmt.Errorf("channels must be >= 1, got %d", channels)
	}
	if len(ref) != len(out) {
		return nil, fmt.Errorf("sample count mismatch: ref has %d, out has %d", len(ref), len(out))
	}
	if len(ref)%channels != 0 {
		return nil, fmt.Errorf("sample count %d is not a multiple of %d channels", len(ref), channels)
	}
	s := &stats{
		channels:  channels,
		framesRef: len(ref) / channels,
		framesOut: len(out) / channels,
		rmsErr:    make([]float64, channels),
		maxErr:    make([]float64, channels),
		maxErrAt:  make([]int, channels),
		peakRef:   make([]float64, channels),
		peakOut:   make([]float64, channels),
	}
	if len(ref) == 0 {
		return s, nil
	}
	sumSq := make([]float64, channels)
	for i, r := range ref {
		o := out[i]
		if math.IsNaN(r) || math.IsInf(r, 0) || math.IsNaN(o) || math.IsInf(o, 0) {
			return nil, fmt.Errorf("NaN or Inf at frame %d channel %d", i/channels, i%channels)
		}
		c := i % channels
		d := math.Abs(o - r)
		sumSq[c] += d * d
		if d > s.maxErr[c] {
			s.maxErr[c] = d
			s.maxErrAt[c] = i / channels
		}
		if a := math.Abs(r); a > s.peakRef[c] {
			s.peakRef[c] = a
		}
		if a := math.Abs(o); a > s.peakOut[c] {
			s.peakOut[c] = a
		}
	}
	frames := float64(len(ref) / channels)
	for c := range sumSq {
		s.rmsErr[c] = math.Sqrt(sumSq[c] / frames)
	}
	return s, nil
}

// quietPeakFullScale 即 -60 dBFS:参考峰值不低于它时按 dB 差判定峰值,更低时按
// 满刻度绝对误差门槛判定。
const quietPeakFullScale = 1e-3

func (s *stats) violations(rmsLimit, maxLimit, peakDBLimit float64) []string {
	var v []string
	for c := 0; c < s.channels; c++ {
		if s.rmsErr[c] > rmsLimit {
			v = append(v, fmt.Sprintf("channel %d: RMS error %g exceeds limit %g", c, s.rmsErr[c], rmsLimit))
		}
		if s.maxErr[c] > maxLimit {
			v = append(v, fmt.Sprintf("channel %d: max absolute error %g at frame %d exceeds limit %g", c, s.maxErr[c], s.maxErrAt[c], maxLimit))
		}
		if s.peakRef[c] >= quietPeakFullScale {
			db := math.Abs(20 * math.Log10(s.peakOut[c]/s.peakRef[c]))
			if db > peakDBLimit {
				v = append(v, fmt.Sprintf("channel %d: sample-peak difference %.4f dB exceeds limit %g dB", c, db, peakDBLimit))
			}
		} else if d := math.Abs(s.peakOut[c] - s.peakRef[c]); d > maxLimit {
			v = append(v, fmt.Sprintf("channel %d: absolute peak difference %g exceeds limit %g (reference peak below -60 dBFS)", c, d, maxLimit))
		}
	}
	return v
}

func loadPCM(path, format string) ([]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var width int
	switch format {
	case "f64le":
		width = 8
	case "f32le":
		width = 4
	default:
		return nil, fmt.Errorf("unknown sample format %q (want f64le or f32le)", format)
	}
	if len(data)%width != 0 {
		return nil, fmt.Errorf("%s: %d bytes is not a multiple of the %d-byte sample size", path, len(data), width)
	}
	n := len(data) / width
	samples := make([]float64, n)
	for i := 0; i < n; i++ {
		if width == 8 {
			samples[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[i*8:]))
		} else {
			samples[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:])))
		}
	}
	return samples, nil
}

func skipFrames(samples []float64, channels, frames int) ([]float64, error) {
	if frames < 0 {
		return nil, fmt.Errorf("negative frame skip %d", frames)
	}
	n := frames * channels
	if n > len(samples) {
		return nil, fmt.Errorf("skip of %d frames exceeds %d available frames", frames, len(samples)/channels)
	}
	return samples[n:], nil
}

func dbfs(v float64) float64 {
	if v == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(v)
}

func run(refPath, outPath, refFormat, outFormat string, channels, skipRef, skipOut, frames int, rmsLimit, maxLimit, peakDBLimit float64, w io.Writer) ([]string, error) {
	ref, err := loadPCM(refPath, refFormat)
	if err != nil {
		return nil, err
	}
	out, err := loadPCM(outPath, outFormat)
	if err != nil {
		return nil, err
	}
	if ref, err = skipFrames(ref, channels, skipRef); err != nil {
		return nil, fmt.Errorf("ref: %w", err)
	}
	if out, err = skipFrames(out, channels, skipOut); err != nil {
		return nil, fmt.Errorf("out: %w", err)
	}
	if frames >= 0 {
		need := frames * channels
		if len(ref) < need || len(out) < need {
			return nil, fmt.Errorf("need %d frames after skips, ref has %d, out has %d", frames, len(ref)/channels, len(out)/channels)
		}
		ref = ref[:need]
		out = out[:need]
	}
	s, err := compare(ref, out, channels)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "channels=%d frames ref=%d out=%d\n", s.channels, s.framesRef, s.framesOut)
	for c := 0; c < s.channels; c++ {
		fmt.Fprintf(w, "channel %d: rms_err=%.3e max_err=%.3e at frame %d peak_ref=%.6f (%.2f dBFS) peak_out=%.6f (%.2f dBFS)\n",
			c, s.rmsErr[c], s.maxErr[c], s.maxErrAt[c],
			s.peakRef[c], dbfs(s.peakRef[c]), s.peakOut[c], dbfs(s.peakOut[c]))
	}
	return s.violations(rmsLimit, maxLimit, peakDBLimit), nil
}

func main() {
	var (
		refPath     = flag.String("ref", "", "reference PCM file (required)")
		outPath     = flag.String("out", "", "candidate PCM file (required)")
		refFormat   = flag.String("ref-format", "f64le", "reference sample format: f64le or f32le")
		outFormat   = flag.String("out-format", "f64le", "candidate sample format: f64le or f32le")
		channels    = flag.Int("channels", 2, "channel count of both files")
		skipRef     = flag.Int("skip-ref", 0, "documented sample frames to skip at the start of the reference")
		skipOut     = flag.Int("skip-out", 0, "documented sample frames to skip at the start of the candidate")
		rmsLimit    = flag.Float64("rms", 1e-5, "per-channel RMS error limit in full scale")
		maxLimit    = flag.Float64("max", 1e-3, "per-channel absolute error limit in full scale")
		peakDBLimit = flag.Float64("peak-db", 0.1, "sample-peak difference limit in dB when reference peak >= -60 dBFS")
		frames      = flag.Int("frames", -1, "if >= 0, compare exactly this many sample frames from each file after skips (both must contain at least this many)")
	)
	flag.Parse()
	if *refPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "pcmdiff: -ref and -out are required")
		os.Exit(2)
	}
	violations, err := run(*refPath, *outPath, *refFormat, *outFormat, *channels, *skipRef, *skipOut, *frames, *rmsLimit, *maxLimit, *peakDBLimit, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pcmdiff:", err)
		os.Exit(2)
	}
	for _, v := range violations {
		fmt.Fprintln(os.Stdout, "FAIL:", v)
	}
	if len(violations) > 0 {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "PASS")
}
