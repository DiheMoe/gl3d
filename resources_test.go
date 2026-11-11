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

// 稳态解码不分配:解码器建好、PCM 队列长到稳定容量之后,逐帧解码与交付不产生堆
// 分配,长流的分配量与时长无关。
func TestSteadyStateDecodingDoesNotAllocate(t *testing.T) {
	emphasis, _ := shineWithEmphasis(t, 0, -1)
	for _, tc := range []struct {
		name string
		data []byte
		opts []Option
	}{
		{"strict", readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3"), []Option{Strict()}},
		{"default", readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3"), nil},
		// 宽松模式下每个 emphasis 非零的帧都要过一遍异常处理,稳态下也不能分配
		{"lenient with emphasis on every frame", emphasis, []Option{lenient()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d, err := NewDecoder(ctx, newLoopReader(tc.data, 100), tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]float64, 1152*d.Channels())
			read := func() {
				if _, err := d.ReadPCM(ctx, buf); err != nil {
					t.Fatal(err)
				}
			}
			for range 20 {
				read()
			}
			if allocs := testing.AllocsPerRun(200, read); allocs != 0 {
				t.Fatalf("%v allocations per ReadPCM in steady state, want 0", allocs)
			}
		})
	}
}

// 内存约束:扣除 Go 运行时、输入语料与调用方缓冲后,单实例峰值存活堆
// ≤ 4 MiB,同格式 5 分钟与 60 分钟流的存活堆增量 ≤ 1 MiB。流由无标签的
// shine 样例(191 帧,约 5 s)首尾相接而成。该样例首帧不引用 reservoir,
// 所以循环拼接后仍是合法流。
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

	d, err := NewDecoder(ctx, r, Strict())
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
		d, err := NewDecoder(context.Background(), r, Strict())
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

// 重新同步:扫过一长段像帧同步、帧头或标签标识的垃圾,检查候选的判断不产生与垃圾长度成
// 正比的堆分配,结果仍然正确。输出缓冲复用,不累积 PCM。
func TestResyncAllocationBounded(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	_, want := decodeAll(t, bytes.NewReader(cat(shine, shine)))
	for _, tc := range []struct {
		name    string
		pattern []byte
		bound   uint64 // 扫过 1 MiB 垃圾允许的堆分配总量
		// 垃圾整段都被跳过且只重新同步一次。否则垃圾里的标签标识会被识别、失败,逐个计入
		// 报告,这时只核对解出的帧数
		skipsAll bool
	}{
		{"sync bytes", []byte{0xFF}, 2 << 20, true},
		// 同格式的有效帧头成簇出现,帧长处及其后的松弛范围内却没有帧头,所以每个候选还要检查
		// 这一段字节。帧头簇的间隔大于帧长加 resyncSlack,否则垃圾会被当作帧交出
		{"valid headers", cat(bytes.Repeat([]byte{0xFF, 0xFB, 0x90, 0x00}, 8), make([]byte, 1024-32)), 2 << 20, true},
		// 除了保留的 emphasis,其余都像有效的帧头
		{"reserved emphasis", []byte{0xFF, 0xFB, 0x90, 0x02}, 2 << 20, true},
		// 头无效的 ID3 标识:每个都要判断头是否有效,判断不分配
		{"ID3 with a size byte over 0x7F", []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0x80}, 2 << 20, true},
		// 下面两行的分配与垃圾长度成正比,所以各自单独给上限,只比实测值高约四分之一。这样
		// 每个标识的分配变多时测试就会失败。LYRICSBEGIN 的每个标识都被识别、读不下去,每个
		// 标识构造一个错误(每次 24 B,约每输入字节 2.2 B),实测约 2.4 MiB,超过其余几行的
		// 2 MiB。TAG 被当作 ID3v1 读完 128 字节,其后不是帧也不是标签,每读完一个就构造错误,
		// 实测约 0.8 MiB
		{"LYRICSBEGIN without an end", []byte("LYRICSBEGIN"), 3 << 20, false},
		{"TAG", []byte("TAG"), 1 << 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 垃圾以零字节开头,否则紧跟在帧后的完整帧头会被当作一个帧交出,而不是
			// 由重新同步扫过。垃圾以零字节结尾,最后一个标识才不会越过垃圾吃掉其后的帧
			junk := cat([]byte{0}, bytes.Repeat(tc.pattern, 1<<20/len(tc.pattern)), make([]byte, 256))
			data := cat(shine, junk, shine)
			ctx := context.Background()
			buf := make([]float64, 4096)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			d, err := NewDecoder(ctx, bytes.NewReader(data), lenient())
			if err != nil {
				t.Fatal(err)
			}
			samples := 0
			for {
				n, err := d.ReadPCM(ctx, buf)
				samples += n * d.Channels()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("scanning %d B of junk allocated %d B", len(junk), allocated)
			if allocated >= tc.bound {
				t.Fatalf("scanning %d B of junk allocated %d B, want less than %d B", len(junk), allocated, tc.bound)
			}
			r := d.Report()
			if samples != len(want) || tc.skipsAll && (r.SkippedBytes != int64(len(junk)) || r.Resyncs != 1) {
				t.Fatalf("%d samples (want %d), report %+v", samples, len(want), r)
			}
		})
	}
}

// decodeSummary 以 opts 选项在任意 goroutine 中完整解码,出错以返回值报告(不调用 t.Fatal)。
func decodeSummary(data []byte, bufFrames int, opts ...Option) (decodeResult, error) {
	ctx := context.Background()
	d, err := NewDecoder(ctx, bytes.NewReader(data), opts...)
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
		"sine440-stereo-44k1-cbr192.mp3",   // MPEG-1 立体声,gapless 裁剪
		"sine880-mono-11k025-cbr32.mp3",    // MPEG-2.5 单声道
		"pluck-stereo-48k-cbr128-lame.mp3", // 短块与表 0
		"sine440-stereo-44k1-cbr192-shine.mp3",
	}
	data := make([][]byte, len(files))
	serial := make([]decodeResult, len(files))
	for i, f := range files {
		data[i] = readCorpus(t, f)
		res, err := decodeSummary(data[i], 4096, Strict())
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
				results[i], errs[i] = decodeSummary(data[i], 1+i*97+round*31, Strict()) // 各实例用不同的输出缓冲
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
