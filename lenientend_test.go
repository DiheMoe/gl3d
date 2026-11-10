package gl3d

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

const toneDelivered = 220500 // sine440-stereo-44k1-cbr192.mp3 完好解码交付的采样帧数

// 帧数多于声明:尾裁区间在声明位置,流中间多出的采样帧丢弃,其后的帧照常交付;
// 严格模式交付同样的样本后报帧数不符,宽松模式记入报告。
func TestTrimAtDeclaredPositionWithExtraFrames(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	starts, end := audioFrames(t, data)
	extra := cat(data[:end], data[starts[0]:starts[10]], data[end:])
	_, strictPCM, err := decodeMode(t, bytes.NewReader(extra), Strict())
	var tr *TruncatedError
	if !errors.As(err, &tr) {
		t.Fatalf("strict: err=%v, want TruncatedError", err)
	}
	d, pcm, err := decodeMode(t, bytes.NewReader(extra), lenient())
	if err != nil {
		t.Fatalf("lenient: %v", err)
	}
	if !slices.Equal(strictPCM, pcm) {
		t.Fatal("strict and lenient deliver different samples")
	}
	r := d.Report()
	if r.First == nil || r.First.Kind != IssueTruncated || r.DeclaredFrames != 193 || r.AudioFrames != 203 {
		t.Fatalf("report %+v first %+v", r, r.First)
	}
}

// ICY 式数据块(长度字节 2,随后 32 字节元数据)插在帧内:宽松模式跳过这 33 字节,
// 这一帧照常交出,每一帧都解出。
func TestLenientICYBlock(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	starts, end := audioFrames(t, data)
	icy := cat([]byte{2}, []byte("StreamTitle='x';"), make([]byte, 16))
	at := starts[50] + 150
	in := cat(data[:at], icy, data[at:end])
	d, pcm, err := decodeMode(t, bytes.NewReader(in), lenient())
	if err != nil {
		t.Fatalf("lenient: %v", err)
	}
	r := d.Report()
	if len(pcm) != toneDelivered*2 || r.AudioFrames != 193 || r.SkippedBytes != 33 || r.Resyncs != 1 || r.Clean() {
		t.Fatalf("delivered %d samples, report %+v", len(pcm), r)
	}
}
