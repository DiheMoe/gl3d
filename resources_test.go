package gl3d

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

// loopReader 把一组首尾可相接的音频帧重复交付 reps 次,不按流长分配内存。
type loopReader struct {
	frames []byte
	off    int
	left   int64
}

func newLoopReader(frames []byte, reps int) *loopReader {
	return &loopReader{frames: frames, left: int64(reps) * int64(len(frames))}
}

func (l *loopReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, io.EOF
	}
	n := copy(p, l.frames[l.off:])
	n = int(min(int64(n), l.left))
	l.off = (l.off + n) % len(l.frames)
	l.left -= int64(n)
	return n, nil
}

// liveHeap 返回 GC 之后的存活堆字节数。
func liveHeap() int64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

// 内存约束:扣除 Go 运行时、输入语料与调用方缓冲后,单实例峰值存活堆
// ≤ 4 MiB;同格式 5 分钟与 60 分钟流的存活堆增量 ≤ 1 MiB。流由无标签的
// shine 样例(191 帧,约 5 s)首尾相接而成;该样例首帧不引用 reservoir,
// 循环拼接后仍是合法流。
func TestDecoderMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("decodes 60 minutes of audio")
	}
	const reps5, reps60 = 60, 722 // 191×1152/44100 ≈ 4.99 s 一轮
	frames := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	r := newLoopReader(frames, reps60)
	buf := make([]float64, 4096)
	ctx := context.Background()
	base := liveHeap() // 输入语料与调用方缓冲已在基线之内

	d, err := NewDecoder(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var peak, at5 int64
	at5Frames := int64(reps5 * 191 * 1152)
	next := int64(0)
	for {
		_, err := d.ReadPCM(ctx, buf)
		if dec := d.DecodedFrames(); dec >= next || (at5 == 0 && dec >= at5Frames) {
			live := liveHeap() - base
			peak = max(peak, live)
			if at5 == 0 && dec >= at5Frames {
				at5 = live
			}
			next = dec + 1000*1152 // 约每 26 s 音频采样一次
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	at60 := liveHeap() - base
	peak = max(peak, at60)
	runtime.KeepAlive(d)
	if want := int64(reps60) * 191 * 1152; d.Frames() != want {
		t.Fatalf("Frames()=%d, want %d", d.Frames(), want)
	}
	t.Logf("decoded %.1f min: peak live heap %d B, at 5 min %d B, at 60 min %d B",
		float64(d.Frames())/44100/60, peak, at5, at60)
	if peak > 4<<20 {
		t.Fatalf("peak live heap %d B exceeds 4 MiB", peak)
	}
	if at60-at5 > 1<<20 {
		t.Fatalf("live heap grew %d B between 5 and 60 minutes, limit 1 MiB", at60-at5)
	}
}

// 标签预算:标签分块跳过,不按标签声明的长度分配内存。前置 12 MiB ID3v2
// 标签时,构造解码器的堆分配总量与无标签时只差常数。
func TestTagSkipAllocationBounded(t *testing.T) {
	audio := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 3)
	tagged := append(id3v2Tag(12<<20), audio...)
	construct := func(data []byte) uint64 {
		r := bytes.NewReader(data)
		before := heapAllocated()
		d, err := NewDecoder(context.Background(), r)
		after := heapAllocated()
		if err != nil {
			t.Fatal(err)
		}
		runtime.KeepAlive(d)
		return after - before
	}
	plain, big := construct(audio), construct(tagged)
	t.Logf("NewDecoder allocated %d B without tag, %d B after a 12 MiB tag", plain, big)
	if big > plain+256<<10 {
		t.Fatalf("12 MiB tag: allocated %d B, %d B more than without tag", big, big-plain)
	}
}

// decodeSummary 在任意 goroutine 中完整解码,出错以返回值报告(不调用 t.Fatal)。
func decodeSummary(data []byte, bufFrames int) (decodeResult, error) {
	ctx := context.Background()
	d, err := NewDecoder(ctx, bytes.NewReader(data))
	if err != nil {
		return decodeResult{}, err
	}
	res := decodeResult{sampleRate: d.SampleRate(), nchans: d.Channels()}
	buf := make([]float64, bufFrames*d.Channels())
	for {
		n, err := d.ReadPCM(ctx, buf)
		res.pcm = append(res.pcm, buf[:n*d.Channels()]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, err
		}
		if n == 0 {
			return res, errors.New("no progress: (0, nil)")
		}
	}
	res.frames, res.decoded, res.trimmed = d.Frames(), d.DecodedFrames(), d.Trimmed()
	return res, nil
}

// 并发约束:4 个实例并发解码的结果与串行逐位一致(实例间无共享状态),
// 解码器不留下 goroutine。数据竞争由 go test -race 检查。
func TestConcurrentDecodersMatchSerial(t *testing.T) {
	files := []string{
		"sine440-stereo-44k1-cbr192.mp3",   // MPEG-1 立体声,gapless 尾部缓冲
		"sine880-mono-11k025-cbr32.mp3",    // MPEG-2.5 单声道
		"pluck-stereo-48k-cbr128-lame.mp3", // 短块与表 0
		"sine440-stereo-44k1-cbr192-shine.mp3",
	}
	data := make([][]byte, len(files))
	serial := make([]decodeResult, len(files))
	for i, f := range files {
		data[i] = readCorpus(t, f)
		res, err := decodeSummary(data[i], 4096)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		serial[i] = res
	}
	before := runtime.NumGoroutine()
	for round := 0; round < 3; round++ {
		results := make([]decodeResult, len(files))
		errs := make([]error, len(files))
		var wg sync.WaitGroup
		for i := range files {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = decodeSummary(data[i], 1+i*97+round*31) // 各实例用不同的输出缓冲
			}()
		}
		wg.Wait()
		for i, f := range files {
			if errs[i] != nil {
				t.Fatalf("round %d %s: %v", round, f, errs[i])
			}
			got, want := results[i], serial[i]
			if got.frames != want.frames || got.decoded != want.decoded || got.trimmed != want.trimmed ||
				!bytes.Equal(float64Bytes(got.pcm), float64Bytes(want.pcm)) {
				t.Fatalf("round %d %s: concurrent decode differs from serial", round, f)
			}
		}
	}
	// 测试框架自身的 goroutine 可能稍后才退出,给一点时间再比较
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatal(fmt.Sprintf("goroutines: %d before, %d after concurrent decoding", before, after))
	}
}
