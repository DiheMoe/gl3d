package gl3d

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func decodeAll(t *testing.T, r io.Reader) (*Decoder, []float64) {
	t.Helper()
	d, err := NewDecoder(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var pcm []float64
	buf := make([]float64, 4096)
	for {
		n, err := d.ReadPCM(context.Background(), buf)
		pcm = append(pcm, buf[:n*d.Channels()]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("no progress: (0, nil) loop")
		}
	}
	return d, pcm
}

// manifestEntry 对应 corpus/manifest.json 的条目,只映射本测试用到的字段。
type manifestEntry struct {
	File           string `json:"file"`
	ExpectedFormat struct {
		SampleRate int `json:"sampleRate"`
		Channels   int `json:"channels"`
	} `json:"expectedFormat"`
	Trim struct {
		Kind string `json:"kind"`
	} `json:"trim"`
	Features       []string `json:"features"`
	ExpectedFrames *int64   `json:"expectedFrames"`
}

func loadManifest(t *testing.T) []manifestEntry {
	t.Helper()
	data, err := os.ReadFile("corpus/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Entries []manifestEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m.Entries
}

// 清单内每个样例都与 ffmpeg 参考输出逐样本比较,并核对格式、交付帧数与
// 裁剪行为;新登记的样例自动纳入。边解码边比较,内存不随样例长度增长;
// 5 分钟样例在 -short 下跳过。
func TestEndToEndLavcCorpus(t *testing.T) {
	for _, e := range loadManifest(t) {
		name := strings.TrimSuffix(filepath.Base(e.File), ".mp3")
		t.Run(name, func(t *testing.T) {
			if testing.Short() && slices.Contains(e.Features, "five-minute") {
				t.Skip("five-minute sample in -short mode")
			}
			ref, err := os.Open(filepath.Join("corpus/synthetic/ref", name+".ffmpeg.f64le"))
			if err != nil {
				if os.Getenv("GL3D_REQUIRE_REFERENCE") == "1" || !errors.Is(err, os.ErrNotExist) {
					t.Fatal("required reference output not available:", err)
				}
				t.Skip("reference output not available (set GL3D_REQUIRE_REFERENCE=1 to require it):", err)
			}
			defer ref.Close()
			src, err := os.Open(e.File)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			d, err := NewDecoder(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			if d.SampleRate() != e.ExpectedFormat.SampleRate || d.Channels() != e.ExpectedFormat.Channels {
				t.Fatalf("format %d Hz × %d ch, manifest %d Hz × %d ch",
					d.SampleRate(), d.Channels(), e.ExpectedFormat.SampleRate, e.ExpectedFormat.Channels)
			}
			rms, max, err := compareStream(d, bufio.NewReaderSize(ref, 1<<16))
			if err != nil {
				t.Fatal(err)
			}
			if e.ExpectedFrames != nil && d.Frames() != *e.ExpectedFrames {
				t.Fatalf("Frames()=%d, manifest expectedFrames=%d", d.Frames(), *e.ExpectedFrames)
			}
			if wantTrim := e.Trim.Kind == "lame-gapless"; d.Trimmed() != wantTrim {
				t.Fatalf("Trimmed()=%v, manifest trim.kind=%q", d.Trimmed(), e.Trim.Kind)
			}
			t.Logf("rms=%.3e max=%.3e frames=%d decoded=%d trimmed=%v", rms, max, d.Frames(), d.DecodedFrames(), d.Trimmed())
			if !(rms <= 1e-5 && max <= 1e-3) {
				t.Fatalf("rms=%g max=%g exceed thresholds", rms, max)
			}
		})
	}
}

// compareStream 边解码边与 f64le 参考逐样本比较,返回 RMS 与最大绝对误差;
// 参考长度必须与解码输出恰好一致。RMS 分母为采样帧数,不小于任一声道的
// RMS;NaN 比较恒为假,不先拦截会使误差门槛失效。
func compareStream(d *Decoder, ref io.Reader) (rms, maxErr float64, err error) {
	buf := make([]float64, 4096*d.Channels())
	var b [8]byte
	var sum float64
	var n int64
	for {
		k, err := d.ReadPCM(context.Background(), buf)
		for _, v := range buf[:k*d.Channels()] {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return 0, 0, fmt.Errorf("decoder sample %d is %v", n, v)
			}
			if _, rerr := io.ReadFull(ref, b[:]); rerr != nil {
				return 0, 0, fmt.Errorf("reference ends at sample %d, decoder continues: %w", n, rerr)
			}
			r := math.Float64frombits(binary.LittleEndian.Uint64(b[:]))
			if math.IsNaN(r) || math.IsInf(r, 0) {
				return 0, 0, fmt.Errorf("reference sample %d is %v", n, r)
			}
			diff := math.Abs(v - r)
			sum += diff * diff
			maxErr = max(maxErr, diff)
			n++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, err
		}
		if k == 0 {
			return 0, 0, errors.New("no progress: (0, nil)")
		}
	}
	if _, err := io.ReadFull(ref, b[:]); err != io.EOF {
		return 0, 0, fmt.Errorf("reference has samples beyond the decoder's %d: %v", n, err)
	}
	if n == 0 {
		return 0, 0, nil
	}
	return math.Sqrt(sum / float64(n/int64(d.Channels()))), maxErr, nil
}

func TestCompareStreamRejectsNonFiniteReference(t *testing.T) {
	data := readCorpus(t, "tone10ms-stereo-44k1-cbr128-lame.mp3")
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			d, err := NewDecoder(context.Background(), bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = compareStream(d, bytes.NewReader(float64Bytes([]float64{v})))
			if err == nil || !strings.Contains(err.Error(), "reference sample 0") {
				t.Fatalf("reference %v: err=%v, want non-finite reference error", v, err)
			}
		})
	}
}

func float64Bytes(v []float64) []byte {
	b := make([]byte, len(v)*8)
	for i, x := range v {
		binary.LittleEndian.PutUint64(b[i*8:], math.Float64bits(x))
	}
	return b
}

// 交付数 220500 = 193×1152 − (576+529) − (1260−529):LAME 标签的 enc_delay
// 与 enc_padding 都要计入解码器 529 样本的结构延迟,裁剪后恰为 5 s × 44100 Hz。
func TestEndToEndCounts(t *testing.T) {
	d, _ := decodeAll(t, bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")))
	if d.Frames() != 220500 {
		t.Fatalf("Frames()=%d, want 220500 (5s @ 44.1kHz, gapless-trimmed)", d.Frames())
	}
	if d.DecodedFrames() != 193*1152 {
		t.Fatalf("DecodedFrames()=%d, want %d", d.DecodedFrames(), 193*1152)
	}
	if !d.Trimmed() {
		t.Fatal("Trimmed()=false, want true (LAME tag present)")
	}
}

// shine 编码器不写 Xing/LAME 标签:无 gapless 信息,不裁剪,交付全部 191 个音频帧。
func TestEndToEndUntrimmed(t *testing.T) {
	d, _ := decodeAll(t, bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")))
	if d.Trimmed() {
		t.Fatal("Trimmed()=true for tagless stream")
	}
	if d.Frames() != 191*1152 {
		t.Fatalf("Frames()=%d, want %d (191 个完整帧)", d.Frames(), 191*1152)
	}
}
