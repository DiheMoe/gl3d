package gl3d

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// decodeVariant 解码变体的字节并返回交付的 PCM,选项由调用方决定。
func decodeVariant(t *testing.T, data []byte, opts ...Option) (*Decoder, []float64, error) {
	t.Helper()
	return decodeMode(t, bytes.NewReader(data), opts...)
}

// decodeUntrimmed 以宽松模式解码 data,不做 gapless 裁剪:第二类变体的尾部按解码流的
// 结尾对齐比较,裁剪区间的位置取决于声明帧数与实际帧数,不能参与对齐。
func decodeUntrimmed(t *testing.T, data []byte) []float64 {
	t.Helper()
	d, err := NewDecoder(context.Background(), bytes.NewReader(data), lenient())
	if err != nil {
		t.Fatal(err)
	}
	d.headTrim, d.tailFrom, d.tailTo, d.badPadding = 0, 0, 0, false
	var pcm []float64
	buf := make([]float64, 4096)
	for {
		n, err := d.ReadPCM(context.Background(), buf)
		pcm = append(pcm, buf[:n*d.Channels()]...)
		if err == io.EOF {
			return pcm
		}
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("no progress: (0, nil) loop")
		}
	}
}

// requireSamplesEqual 要求两段交错 PCM 逐样本完全相同;不同时报出第一处差异的位置与差异个数。
func requireSamplesEqual(t *testing.T, what string, got, want []float64, ch int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d samples, want %d", what, len(got), len(want))
	}
	first, n := -1, 0
	for i := range got {
		if got[i] != want[i] {
			if first < 0 {
				first = i
			}
			n++
		}
	}
	if n > 0 {
		t.Errorf("%s: %d of %d samples differ, first at sample frame %d channel %d (%v, want %v)",
			what, n, len(got), first/ch, first%ch, got[first], want[first])
	}
}

// 第一类变体(音频完好、外围结构异常):宽松模式成功,输出全部有限,交付清单给出的采样
// 帧数;pcm 的种类(intact、prefix、extra、offset)见 variantExpect.PCM。
func TestVariantsClass1(t *testing.T) {
	for _, v := range loadVariants(t) {
		if v.Class != 1 {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			skipHugeInShort(t, v)
			base, starts, _ := variantBase(t, v.Base)
			spf, ch, lsf := frameShape(t, base, starts)
			_, intact, err := decodeVariant(t, base, lenient())
			if err != nil {
				t.Fatalf("intact base: %v", err)
			}

			data := variantBytes(t, v)
			d, pcm, err := decodeVariant(t, data, lenient())
			if err != nil {
				t.Fatalf("lenient: %v", err)
			}
			if got := d.Frames(); got != v.Expect.Frames {
				t.Errorf("frames %d, want %d", got, v.Expect.Frames)
			}
			if want := int(v.Expect.Frames) * d.Channels(); len(pcm) != want {
				t.Errorf("%d samples, want %d", len(pcm), want)
			}
			for i, s := range pcm {
				if math.IsNaN(s) || math.IsInf(s, 0) {
					t.Fatalf("sample %d is %v", i, s)
				}
			}

			switch v.Expect.PCM {
			case "intact":
				if !slices.Equal(pcm, intact) {
					t.Error("pcm differs from the intact file's")
				}
			case "prefix":
				n := min(len(pcm), len(intact))
				if !slices.Equal(pcm[:n], intact[:n]) {
					t.Errorf("pcm differs from the intact file's within the first %d samples", n)
				}
			case "extra":
				// 尾部填充在声明位置丢弃,其后恰是复制进来的各帧。它们的前几帧回指的 main data
				// 在流的最后一帧里而不是原来的前一帧,所以 MPEG-1 从第 3 帧起、LSF(帧更短,
				// 回指跨更多帧)从第 5 帧起才与原帧的解码逐样本相同
				op := v.Ops[0]
				n := len(intact) / ch
				if len(pcm) < len(intact) {
					t.Fatalf("%d samples, fewer than the intact file's %d", len(pcm), len(intact))
				}
				requireSamplesEqual(t, "up to the declared end", pcm[:len(intact)], intact, ch)
				if got, want := len(pcm)/ch-n, op.N*spf; got != want {
					t.Fatalf("%d sample frames after the declared end, want %d (%d copied frames)", got, want, op.N)
				}
				skip := 2
				if lsf {
					skip = 4
				}
				u := decodeUntrimmed(t, base)
				if skip >= op.N || (op.Frame+op.N)*spf*ch > len(u) {
					t.Fatalf("copied frames [%d, %d) cannot be compared after skipping %d", op.Frame, op.Frame+op.N, skip)
				}
				requireSamplesEqual(t, "copied frames",
					pcm[(n+skip*spf)*ch:(n+op.N*spf)*ch], u[(op.Frame+skip)*spf*ch:(op.Frame+op.N)*spf*ch], ch)
			case "offset":
				// 从头丢掉 N 帧:第一帧的 main data 随被丢弃的帧一起没了,从第二帧起交付的第 j 个
				// 采样帧就是完好输出的第 j+N·spf 个;完好输出多出的尾部填充被裁掉,比较到重叠处为止
				op := v.Ops[0]
				shift := op.N * spf
				lo, hi := spf, min(len(pcm)/ch, len(intact)/ch-shift)
				if lo >= hi {
					t.Fatalf("no overlap to compare: [%d, %d)", lo, hi)
				}
				requireSamplesEqual(t, "shifted output", pcm[lo*ch:hi*ch], intact[(lo+shift)*ch:(hi+shift)*ch], ch)
			default:
				t.Fatalf("pcm %q", v.Expect.PCM)
			}
		})
	}
}

// 固定每个变体的严格模式结果类别。每个变体还要通过跨模式的完整判定(streamOnce):
// 整体与分块读取一致,模式等价(严格模式成功当且仅当宽松模式的报告干净,此时输出相同),
// 宽松模式不以截断或损坏结束。
func TestVariantsStrict(t *testing.T) {
	for _, v := range loadVariants(t) {
		t.Run(v.Name, func(t *testing.T) {
			skipHugeInShort(t, v)
			data := variantBytes(t, v)
			withLimits(t, func() {
				if got := streamOnce(t, data)[0].class; got != v.Expect.Strict {
					t.Errorf("strict class %s, want %s", got, v.Expect.Strict)
				}
			})
		})
	}
}

// channelErrors 返回两段交错 PCM 逐声道的 RMS 误差与最大绝对误差。
func channelErrors(got, want []float64, ch int) (rms, maxAbs []float64) {
	rms, maxAbs = make([]float64, ch), make([]float64, ch)
	for i := range got {
		e := math.Abs(got[i] - want[i])
		rms[i%ch] += e * e
		maxAbs[i%ch] = max(maxAbs[i%ch], e)
	}
	for c := range rms {
		rms[c] = math.Sqrt(rms[c] / float64(len(got)/ch))
	}
	return rms, maxAbs
}

// 第二类变体(音频损坏与拼接):宽松模式不失败,输出全部有限;交付长度与 FFmpeg 相差不超过
// 2 帧;第一处损坏之前的输出与完好文件逐字节相同;最后一处损坏之后第 3 帧起,按解码流
// (尾裁之前)的结尾对齐,与尾部来源文件的解码在门槛内一致。
func TestVariantsClass2(t *testing.T) {
	const rmsLimit, maxLimit = 1e-5, 1e-3
	for _, v := range loadVariants(t) {
		if v.Class != 2 {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			base, starts, _ := variantBase(t, v.Base)
			perFrame, ch, _ := frameShape(t, base, starts)
			spf := int64(perFrame)

			data := variantBytes(t, v)
			d, pcm, err := decodeVariant(t, data, lenient())
			if err != nil {
				t.Fatalf("lenient: %v", err)
			}
			for i, s := range pcm {
				if math.IsNaN(s) || math.IsInf(s, 0) {
					t.Fatalf("sample %d is %v", i, s)
				}
			}
			diff := d.Frames() - v.Expect.FFmpeg
			if diff < 0 {
				diff = -diff
			}
			if diff > 2*spf {
				t.Errorf("delivered %d frames, ffmpeg %d: differ by %d, over 2 frames of %d", d.Frames(), v.Expect.FFmpeg, diff, spf)
			}

			// 前缀:第一处损坏之前的帧不受影响,头裁之后的输出与完好文件相同
			bd, err := NewDecoder(context.Background(), bytes.NewReader(base), lenient())
			if err != nil {
				t.Fatal(err)
			}
			_, intact, err := decodeVariant(t, base, lenient())
			if err != nil {
				t.Fatalf("intact base: %v", err)
			}
			// base 自己的尾裁可能使完好输出在 firstDamage 之前结束(例如拼接),此时它整体都是前缀
			n := min(max(int64(v.Expect.FirstDamage)*spf-bd.headTrim, 0), int64(len(intact)/ch))
			if int64(len(pcm)) < n*int64(ch) {
				t.Fatalf("prefix %d frames, but the variant has %d samples", n, len(pcm))
			}
			if !slices.Equal(pcm[:n*int64(ch)], intact[:n*int64(ch)]) {
				t.Errorf("pcm differs from the intact file's within the first %d frames", n)
			}

			if v.Expect.TailFrom == nil {
				t.Logf("delivered %d (ffmpeg %d, diff %d), prefix %d frames, no tail", d.Frames(), v.Expect.FFmpeg, diff, n)
				return
			}
			tb := v.Expect.TailBase
			if tb == "" {
				tb = v.Base
			}
			tbData, tbStarts, _ := variantBase(t, tb)
			l := (len(tbStarts) - *v.Expect.TailFrom) * int(spf) * ch
			got, want := decodeUntrimmed(t, data), decodeUntrimmed(t, tbData)
			if len(got) < l || len(want) < l {
				t.Fatalf("tail %d samples, but variant decodes %d and %s %d", l, len(got), tb, len(want))
			}
			rms, maxAbs := channelErrors(got[len(got)-l:], want[len(want)-l:], ch)
			t.Logf("delivered %d (ffmpeg %d, diff %d), prefix %d frames, tail %d frames: rms %.3g, max %.3g", d.Frames(), v.Expect.FFmpeg, diff, n, l/ch, rms, maxAbs)
			for c := range ch {
				if rms[c] > rmsLimit || maxAbs[c] > maxLimit {
					t.Errorf("channel %d tail: rms %.3g (limit %g), max %.3g (limit %g)", c, rms[c], rmsLimit, maxAbs[c], maxLimit)
				}
			}
		})
	}
}

// 两个参考长度一致的第一类变体,宽松模式的输出与 FFmpeg 参考逐样本在门槛内一致。参考
// 由 refgen 生成在 corpus/variants/ref/(见 TestWriteVariants),不存在时跳过。
func TestVariantReferences(t *testing.T) {
	for _, v := range loadVariants(t) {
		if v.Class != 1 || v.InTestOnly || v.Expect.FFmpeg != v.Expect.MPG123 {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			ref, err := os.Open(filepath.Join("corpus/variants/ref", v.Name+".ffmpeg.f64le"))
			if err != nil {
				t.Skip("reference output not available:", err)
			}
			defer ref.Close()
			d, err := NewDecoder(context.Background(), bytes.NewReader(variantBytes(t, v)), lenient())
			if err != nil {
				t.Fatal(err)
			}
			rms, maxErr, err := compareStream(d, bufio.NewReaderSize(ref, 1<<16))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("rms=%.3e max=%.3e frames=%d", rms, maxErr, d.Frames())
			if !(rms <= 1e-5 && maxErr <= 1e-3) {
				t.Fatalf("rms=%g max=%g exceed thresholds", rms, maxErr)
			}
		})
	}
}
