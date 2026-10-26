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
	framesRef int64
	framesOut int64
	rmsErr    []float64
	maxErr    []float64
	maxErrAt  []int64
	peakRef   []float64
	peakOut   []float64
}

const pcmBlockSamples = 4096

// compareReaders keeps two byte blocks and per-channel statistics, regardless
// of input duration. A nonnegative frames limits reads as well as comparison.
func compareReaders(ref, out io.Reader, refWidth, outWidth, channels, frames int) (*stats, error) {
	if channels < 1 {
		return nil, fmt.Errorf("channels must be >= 1, got %d", channels)
	}
	limit := int64(-1)
	if frames >= 0 {
		var err error
		limit, err = frameSamples(frames, channels)
		if err != nil {
			return nil, err
		}
	}
	s := &stats{
		channels: channels,
		rmsErr:   make([]float64, channels),
		maxErr:   make([]float64, channels),
		maxErrAt: make([]int64, channels),
		peakRef:  make([]float64, channels),
		peakOut:  make([]float64, channels),
	}
	var rb, ob [pcmBlockSamples * 8]byte
	var samples int64
	for limit < 0 || samples < limit {
		want := pcmBlockSamples
		if limit >= 0 && limit-samples < int64(want) {
			want = int(limit - samples)
		}
		nr, err := readPCMBlock(ref, rb[:want*refWidth], refWidth)
		if err != nil {
			return nil, fmt.Errorf("ref: %w", err)
		}
		no, err := readPCMBlock(out, ob[:want*outWidth], outWidth)
		if err != nil {
			return nil, fmt.Errorf("out: %w", err)
		}
		if nr != no {
			return nil, fmt.Errorf("sample count mismatch after %d samples: ref has %d, out has %d in the next block", samples, nr, no)
		}
		for i := 0; i < nr; i++ {
			r := pcmValue(rb[i*refWidth:], refWidth)
			o := pcmValue(ob[i*outWidth:], outWidth)
			index := samples + int64(i)
			c := int(index % int64(channels))
			if math.IsNaN(r) || math.IsInf(r, 0) || math.IsNaN(o) || math.IsInf(o, 0) {
				return nil, fmt.Errorf("NaN or Inf at frame %d channel %d", index/int64(channels), c)
			}
			d := math.Abs(o - r)
			s.rmsErr[c] += d * d
			if d > s.maxErr[c] {
				s.maxErr[c] = d
				s.maxErrAt[c] = index / int64(channels)
			}
			s.peakRef[c] = max(s.peakRef[c], math.Abs(r))
			s.peakOut[c] = max(s.peakOut[c], math.Abs(o))
		}
		samples += int64(nr)
		if nr < want {
			break
		}
	}
	if limit >= 0 && samples != limit {
		return nil, fmt.Errorf("need %d frames after skips, both inputs have %d", frames, samples/int64(channels))
	}
	if samples%int64(channels) != 0 {
		return nil, fmt.Errorf("sample count %d is not a multiple of %d channels", samples, channels)
	}
	s.framesRef, s.framesOut = samples/int64(channels), samples/int64(channels)
	if s.framesRef > 0 {
		for c := range s.rmsErr {
			s.rmsErr[c] = math.Sqrt(s.rmsErr[c] / float64(s.framesRef))
		}
	}
	return s, nil
}

func readPCMBlock(r io.Reader, b []byte, width int) (int, error) {
	n, err := io.ReadFull(r, b)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, err
	}
	if n%width != 0 {
		return 0, fmt.Errorf("truncated %d-byte PCM sample (%d bytes read)", width, n)
	}
	return n / width, nil
}

func pcmValue(b []byte, width int) float64 {
	if width == 8 {
		return math.Float64frombits(binary.LittleEndian.Uint64(b))
	}
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
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

func openPCM(path, format string) (*os.File, int, error) {
	var width int
	switch format {
	case "f64le":
		width = 8
	case "f32le":
		width = 4
	default:
		return nil, 0, fmt.Errorf("unknown sample format %q (want f64le or f32le)", format)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	// Preserve the file-size check even when -frames selects only a prefix.
	if fi.Mode().IsRegular() && fi.Size()%int64(width) != 0 {
		f.Close()
		return nil, 0, fmt.Errorf("%s: %d bytes is not a multiple of the %d-byte sample size", path, fi.Size(), width)
	}
	return f, width, nil
}

func frameSamples(frames, channels int) (int64, error) {
	if channels < 1 {
		return 0, fmt.Errorf("channels must be >= 1, got %d", channels)
	}
	if frames < 0 {
		return 0, fmt.Errorf("negative frame count %d", frames)
	}
	if int64(frames) > math.MaxInt64/int64(channels) {
		return 0, fmt.Errorf("frame count %d overflows %d channels", frames, channels)
	}
	return int64(frames) * int64(channels), nil
}

func skipPCM(r io.Reader, width, channels, frames int) error {
	samples, err := frameSamples(frames, channels)
	if err != nil {
		return err
	}
	if samples > math.MaxInt64/int64(width) {
		return fmt.Errorf("frame skip %d overflows byte offset", frames)
	}
	n := samples * int64(width)
	if n == 0 {
		return nil
	}
	if f, ok := r.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			off, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}
			if n > fi.Size()-off {
				return fmt.Errorf("skip of %d frames exceeds %d available frames", frames, (fi.Size()-off)/int64(width)/int64(channels))
			}
			_, err = f.Seek(n, io.SeekCurrent)
			return err
		}
	}
	if _, err := io.CopyN(io.Discard, r, n); err != nil {
		return fmt.Errorf("skip of %d frames exceeds available PCM: %w", frames, err)
	}
	return nil
}

func dbfs(v float64) float64 {
	if v == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(v)
}

func run(refPath, outPath, refFormat, outFormat string, channels, skipRef, skipOut, frames int, rmsLimit, maxLimit, peakDBLimit float64, w io.Writer) ([]string, error) {
	ref, refWidth, err := openPCM(refPath, refFormat)
	if err != nil {
		return nil, err
	}
	defer ref.Close()
	out, outWidth, err := openPCM(outPath, outFormat)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	if err = skipPCM(ref, refWidth, channels, skipRef); err != nil {
		return nil, fmt.Errorf("ref: %w", err)
	}
	if err = skipPCM(out, outWidth, channels, skipOut); err != nil {
		return nil, fmt.Errorf("out: %w", err)
	}
	s, err := compareReaders(ref, out, refWidth, outWidth, channels, frames)
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
