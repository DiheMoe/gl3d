package gl3d

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// 借用接口与复制接口可交替使用,不丢样本,且共享 gapless、计数与终止语义。
func TestNextPCMMatchesReadPCM(t *testing.T) {
	files := []string{
		"short-tone-stereo-44k1-cbr192.mp3",
		"sine440-stereo-44k1-cbr192.mp3",
		"sine880-mono-11k025-cbr32.mp3",
		"pluck-stereo-8k-cbr16-lame.mp3",
		"sine440-stereo-44k1-cbr192-shine.mp3",
	}
	for _, file := range files {
		data := readCorpus(t, file)
		want := decodeWith(t, bytes.NewReader(data), 4096)
		if want.err != nil {
			t.Fatal(want.err)
		}
		for _, mixed := range []bool{false, true} {
			name := file + "/borrowed"
			if mixed {
				name = file + "/mixed-onebyte"
			}
			t.Run(name, func(t *testing.T) {
				var r io.Reader = bytes.NewReader(data)
				if mixed {
					r = &oneByteReader{r: bytes.NewReader(data)}
				}
				d, err := NewDecoder(context.Background(), r)
				if err != nil {
					t.Fatal(err)
				}
				var got []float64
				buf := make([]float64, 7*d.Channels()+1)
				for call := 0; ; call++ {
					var pcm []float64
					if mixed && call%2 == 0 {
						var n int
						n, err = d.ReadPCM(context.Background(), buf)
						pcm = buf[:n*d.Channels()]
					} else {
						pcm, err = d.NextPCM(context.Background())
						if cap(pcm) != len(pcm) {
							t.Fatalf("borrowed cap=%d, len=%d", cap(pcm), len(pcm))
						}
					}
					if len(pcm)%d.Channels() != 0 || err == nil && len(pcm) == 0 {
						t.Fatalf("invalid result: %d samples, %v", len(pcm), err)
					}
					// 在下一次调用前复制,不延长借用视图的生命周期。
					got = append(got, pcm...)
					if d.Frames() != int64(len(got)/d.Channels()) {
						t.Fatalf("Frames=%d after %d samples", d.Frames(), len(got))
					}
					if err != nil {
						if err != io.EOF {
							t.Fatal(err)
						}
						break
					}
				}
				if !bytes.Equal(float64Bytes(got), float64Bytes(want.pcm)) {
					t.Fatal("PCM differs from ReadPCM")
				}
				if d.Frames() != want.frames || d.DecodedFrames() != want.decoded || d.Trimmed() != want.trimmed {
					t.Fatalf("counts=(%d,%d,%v), want (%d,%d,%v)",
						d.Frames(), d.DecodedFrames(), d.Trimmed(), want.frames, want.decoded, want.trimmed)
				}
				for range 2 {
					if pcm, err := d.NextPCM(context.Background()); pcm != nil || err != io.EOF {
						t.Fatalf("after EOF: %d samples, %v", len(pcm), err)
					}
				}
			})
		}
	}
}

func TestNextPCMDrainsBeforeSourceError(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	sourceErr := errors.New("source reset")
	reader := func() io.Reader { return &dataWithErrReader{r: bytes.NewReader(data), err: sourceErr} }
	want := decodeWith(t, reader(), 4096)
	if !errors.Is(want.err, sourceErr) {
		t.Fatalf("ReadPCM error=%v", want.err)
	}
	d, err := NewDecoder(context.Background(), reader())
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	for {
		pcm, err := d.NextPCM(context.Background())
		got = append(got, pcm...)
		if err != nil {
			if pcm != nil || !errors.Is(err, sourceErr) {
				t.Fatalf("terminal result: %d samples, %v", len(pcm), err)
			}
			break
		}
		if len(pcm) == 0 {
			t.Fatal("empty successful result")
		}
	}
	if !bytes.Equal(float64Bytes(got), float64Bytes(want.pcm)) || d.Frames() != want.frames {
		t.Fatal("source error discarded pending PCM")
	}
	if pcm, err := d.NextPCM(context.Background()); pcm != nil || !errors.Is(err, sourceErr) {
		t.Fatalf("sticky error: %d samples, %v", len(pcm), err)
	}
}

func TestNextPCMCancelled(t *testing.T) {
	d, err := NewDecoder(context.Background(), bytes.NewReader(readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.NextPCM(context.Background()); err != nil {
		t.Fatal(err)
	}
	decoded := d.DecodedFrames()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for calls := 0; ; calls++ {
		pcm, err := d.NextPCM(ctx)
		if d.DecodedFrames() != decoded {
			t.Fatal("decoded another frame after cancellation")
		}
		if err != nil {
			if pcm != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled result: %d samples, %v", len(pcm), err)
			}
			break
		}
		if len(pcm) == 0 || calls > 1 {
			t.Fatal("pending PCM was not drained in bounded calls")
		}
	}
}

func TestPCMDeliverySteadyAllocs(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	for _, borrowed := range []bool{false, true} {
		name := "ReadPCM"
		if borrowed {
			name = "NextPCM"
		}
		t.Run(name, func(t *testing.T) {
			d, err := NewDecoder(context.Background(), newLoopReader(data, 100))
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]float64, 4096)
			if allocations := testing.AllocsPerRun(100, func() {
				if borrowed {
					_, err = d.NextPCM(context.Background())
				} else {
					_, err = d.ReadPCM(context.Background(), buf)
				}
				if err != nil {
					t.Fatal(err)
				}
			}); allocations != 0 {
				t.Fatalf("%g allocations per call, want zero after initialization", allocations)
			}
		})
	}
}
