package gl3d

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"runtime/metrics"
	"testing"
	"time"
)

// fuzz 目标分工:FuzzParseTags 覆盖标签与帧头解析;FuzzFrameData 在一组合法
// 帧头下变异 side information 与 main data(帧数据处理);FuzzStream 覆盖任意
// 字节流的完整流式解码。

// fuzzResult 是一次完整解码的摘要:PCM 只保留 FNV-1a 哈希,不留存输出,使单个
// 输入的内存只反映解码器本身。
type fuzzResult struct {
	class                    string // errClass 的终止类别
	frames, decoded, samples int64
	trimmed                  bool
	nchans                   int
	hash                     uint64
}

// fuzzDecode 以 bufFrames 个采样帧的输出缓冲完整解码 r,并检查任何输入都必须
// 成立的不变量:错误归入规定类别(无音频、不支持、截断、损坏、资源限制)、
// 输出无 NaN/Inf、不出现无进展的 (0, nil)、交付样本数与计数自洽。
func fuzzDecode(t *testing.T, r io.Reader, bufFrames int) fuzzResult {
	t.Helper()
	ctx := context.Background()
	res := fuzzResult{hash: 14695981039346656037}
	finish := func(err error) fuzzResult {
		switch res.class = errClass(err); res.class {
		case "ok", "noaudio", "unsupported", "truncated", "corrupt", "limit":
		default:
			t.Fatalf("unclassified error: %s", res.class)
		}
		return res
	}
	d, err := NewDecoder(ctx, r)
	if err != nil {
		return finish(err)
	}
	res.nchans = d.Channels()
	buf := make([]float64, bufFrames*res.nchans)
	for {
		n, err := d.ReadPCM(ctx, buf)
		for _, v := range buf[:n*res.nchans] {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("sample %d is %v", res.samples, v)
			}
			bits := math.Float64bits(v)
			for i := 0; i < 8; i++ {
				res.hash = (res.hash ^ bits&0xFF) * 1099511628211
				bits >>= 8
			}
			res.samples++
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			res.frames, res.decoded, res.trimmed = d.Frames(), d.DecodedFrames(), d.Trimmed()
			if res.samples != res.frames*int64(res.nchans) || res.frames > res.decoded {
				t.Fatalf("delivered %d samples, Frames()=%d × %d channels, DecodedFrames()=%d",
					res.samples, res.frames, res.nchans, res.decoded)
			}
			return finish(err)
		}
		if n == 0 {
			t.Fatal("no progress: (0, nil)")
		}
	}
}

// fuzzInputDeadline 是单个 fuzz 输入的执行上限。Go fuzz 引擎不会识别卡住的
// 输入,这里由独立 goroutine 计时:超时即 panic 使 worker 崩溃,引擎据此保存
// 该输入为 crasher;不依赖被测解码器自己的取消检查。
const fuzzInputDeadline = 10 * time.Second

// fuzzAllocBudget 是单个 fuzz 输入允许的堆分配总量。一个解码器实例构造时约
// 分配 200 KB,稳态解码不分配;预算留出量级余量,超出即视为失控分配并作为
// 可复现的 crasher 保存。
const fuzzAllocBudget = 16 << 20

func heapAllocated() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// withLimits 在单输入的时间期限与分配预算下运行 fn。
func withLimits(t *testing.T, fn func()) {
	t.Helper()
	timer := time.AfterFunc(fuzzInputDeadline, func() {
		panic(fmt.Sprintf("fuzz input still running after %v: hang", fuzzInputDeadline))
	})
	defer timer.Stop()
	before := heapAllocated()
	fn()
	if a := heapAllocated() - before; a > fuzzAllocBudget {
		t.Fatalf("input allocated %d bytes, budget %d", a, fuzzAllocBudget)
	}
}

func fuzzSeedFile(f *testing.F, name string, limit int) []byte {
	f.Helper()
	data, err := os.ReadFile("corpus/synthetic/" + name)
	if err != nil {
		f.Fatal(err)
	}
	return data[:min(limit, len(data))]
}

func FuzzParseTags(f *testing.F) {
	f.Add(rawID3v2(4, 0x10, []byte("TIT2\x00\x00\x00\x05\x00\x00\x03gl3d"), true))
	for _, name := range []string{"short-tone-stereo-44k1-cbr192.mp3", "sine880-mono-11k025-cbr32.mp3", "pluck-stereo-12k-cbr32-lame.mp3"} {
		data := fuzzSeedFile(f, name, 1024)
		f.Add(data)
		if i := bytes.Index(data, []byte("Info")); i >= 0 {
			f.Add(data[i:]) // 从 Xing magic 起的 Info 头与 LAME 扩展
		}
	}
	f.Add(vbriStream(2, 2)[36:417])
	f.Fuzz(func(t *testing.T, b []byte) { withLimits(t, func() { parseTagsOnce(t, b) }) })
}

func parseTagsOnce(t *testing.T, b []byte) {
	if len(b) >= 10 {
		if size, _, err := parseID3v2Header([10]byte(b)); err == nil && (size < 10 || size >= 10+1<<28) {
			t.Fatalf("ID3v2 size %d outside syncsafe range", size)
		}
	}
	if len(b) >= 4 {
		parseFrameHeader([4]byte(b))
	}
	if _, off, err := parseXing(b); err == nil {
		if off < 8 || off > len(b) {
			t.Fatalf("LAME offset %d outside [8, %d]", off, len(b))
		}
		if _, ok := parseLAMETag(b[off:]); ok {
			infoTagCRCValid(b, off+34) // 与 readMetaFrame 相同的前置条件:LAME 扩展完整
		}
	}
	parseVBRI(b)
}

// 全部 2^21 种同步字之后的帧头取值:解析成功的帧头字段自洽,且帧长容得下
// 帧头、CRC 与 side information(帧循环据此切片);失败只归为损坏或不支持,
// 不支持仅在没有保留值时出现。
func TestParseFrameHeaderAllValues(t *testing.T) {
	rates := map[Version][]int{MPEG1: {44100, 48000, 32000}, MPEG2: {22050, 24000, 16000}, MPEG25: {11025, 12000, 8000}}
	for v := 0; v < 1<<21; v++ {
		b := [4]byte{0xFF, 0xE0 | byte(v>>16), byte(v >> 8), byte(v)}
		h, err := parseFrameHeader(b)
		if err != nil {
			var u *UnsupportedError
			var c *CorruptError
			switch {
			case errors.As(err, &u):
				layer, br, sr, emph := b[1]>>1&3, b[2]>>4, b[2]>>2&3, b[3]&3
				if b[1]>>3&3 == 1 || layer == 0 || br == 15 || sr == 3 || emph == 2 {
					t.Fatalf("%x: reserved field reported as unsupported (%v)", b, err)
				}
			case !errors.As(err, &c):
				t.Fatalf("%x: unclassified error %v", b, err)
			}
			continue
		}
		crc := 0
		if h.hasCRC {
			crc = 2
		}
		spf := 1152
		if h.version.lsf() {
			spf = 576
		}
		switch {
		case h.frameSize < 4+crc+h.sideInfoSize:
			t.Fatalf("%x: frameSize %d < header+CRC+side info %d", b, h.frameSize, 4+crc+h.sideInfoSize)
		case h.samplesPerFrame != spf || h.bitrateKbps <= 0:
			t.Fatalf("%x: samplesPerFrame %d bitrate %d", b, h.samplesPerFrame, h.bitrateKbps)
		case rates[h.version][h.srIdx] != h.sampleRate:
			t.Fatalf("%x: sample rate %d for version %v index %d", b, h.sampleRate, h.version, h.srIdx)
		}
	}
}

// fuzzFrameHeaders 是 FuzzFrameData 的帧头:覆盖三个版本、单/双声道、MS 与
// intensity stereo(mode extension)、有无 CRC 与高低码率。
var fuzzFrameHeaders = [][4]byte{
	mkHeader(3, 1, 1, 11, 0, 0, 1, 2, 0), // MPEG-1 192k 44.1k joint MS
	mkHeader(3, 1, 0, 9, 1, 0, 1, 3, 0),  // MPEG-1 128k 48k joint MS+IS,CRC
	mkHeader(3, 1, 1, 5, 2, 0, 3, 0, 0),  // MPEG-1 64k 32k mono
	mkHeader(3, 1, 0, 14, 0, 1, 2, 0, 0), // MPEG-1 320k 44.1k dual,CRC
	mkHeader(2, 1, 1, 8, 1, 0, 1, 1, 0),  // MPEG-2 64k 24k joint IS
	mkHeader(2, 1, 0, 4, 2, 0, 3, 0, 0),  // MPEG-2 32k 16k mono,CRC
	mkHeader(0, 1, 1, 4, 0, 0, 1, 3, 0),  // MPEG-2.5 32k 11.025k joint MS+IS
	mkHeader(0, 1, 0, 2, 2, 0, 0, 0, 0),  // MPEG-2.5 16k 8k stereo,CRC
}

// buildFrames 用 sel 选定的帧头把 body 切成至多 4 帧(不足补 0),有 CRC 时按
// 帧头第 16–31 位与 side information 写入正确的 CRC,使变异越过 CRC 校验进入
// 解码核心。
func buildFrames(sel byte, body []byte) []byte {
	hb := fuzzFrameHeaders[int(sel)%len(fuzzFrameHeaders)]
	h, err := parseFrameHeader(hb)
	if err != nil {
		panic(err)
	}
	var out []byte
	for i := 0; i < 4 && (i == 0 || len(body) > 0); i++ {
		frame := make([]byte, h.frameSize)
		copy(frame, hb[:])
		off := 4
		if h.hasCRC {
			off = 6
		}
		n := copy(frame[off:], body)
		body = body[n:]
		if h.hasCRC {
			c := crc16Update(crc16(frame[2:4]), frame[6:6+h.sideInfoSize])
			frame[4], frame[5] = byte(c>>8), byte(c)
		}
		out = append(out, frame...)
	}
	return out
}

func FuzzFrameData(f *testing.F) {
	pluck := fuzzSeedFile(f, "pluck-stereo-48k-cbr128-lame.mp3", 8<<10)
	sine := fuzzSeedFile(f, "sine440-stereo-44k1-cbr192.mp3", 8<<10)
	for sel := range fuzzFrameHeaders {
		f.Add(byte(sel), []byte{})
		f.Add(byte(sel), pluck[len(pluck)/2:])
		f.Add(byte(sel), sine[len(sine)/2:])
	}
	f.Fuzz(func(t *testing.T, sel byte, body []byte) {
		withLimits(t, func() { fuzzDecode(t, bytes.NewReader(buildFrames(sel, body)), 4096) })
	})
}

func FuzzStream(f *testing.F) {
	for _, name := range []string{
		"short-tone-stereo-44k1-cbr192.mp3",
		"tone10ms-stereo-44k1-cbr128-lame.mp3",
		"pluck-stereo-8k-cbr16-lame.mp3",
	} {
		f.Add(fuzzSeedFile(f, name, 1<<20))
	}
	for _, name := range []string{
		"pluck-stereo-48k-cbr128-lame.mp3",
		"pluck-stereo-16k-cbr48-crc-lame.mp3",
		"sine880-mono-11k025-cbr32.mp3",
		"sine440-jstereo-44k1-cbr128-shine.mp3",
	} {
		f.Add(fuzzSeedFile(f, name, 3<<10))
	}
	f.Add(append(fuzzSeedFile(f, "tone10ms-stereo-44k1-cbr128-lame.mp3", 1<<20), id3v1Tag()...))
	f.Add(repeatFrames(mkHeader(3, 2, 1, 10, 0, 0, 0, 0, 0), 626, 3)) // Layer II
	f.Add(vbriStream(2, 2))
	f.Fuzz(func(t *testing.T, data []byte) { withLimits(t, func() { streamOnce(t, data) }) })
}

func streamOnce(t *testing.T, data []byte) {
	whole := fuzzDecode(t, bytes.NewReader(data), 4096)
	// 输入分块方式与输出缓冲大小不得改变解码结果
	r := &chunkReader{data: data, rng: rand.New(rand.NewSource(int64(len(data)))), max: 1 + len(data)%97}
	if chunked := fuzzDecode(t, r, 1+len(data)%13); chunked != whole {
		t.Fatalf("chunked decode %+v differs from whole decode %+v", chunked, whole)
	}
}
