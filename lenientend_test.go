package gl3d

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

const toneDelivered = 220500 // sine440-stereo-44k1-cbr192.mp3 完好解码交付的采样帧数

// 帧数多于声明:尾裁区间在声明位置,流中间那 731 个采样帧丢弃,其后的帧照常交付;
// 严格模式交付同样的样本后报帧数不符,宽松模式记入报告。接上的是开头 10 帧:第一帧
// main_data_begin 为 0,这 10 帧解码时回指的字节与原来相同,不会出现解码错误。丢弃的位置
// 由接上的 10 帧钉住:它们(第一帧除外,其重叠相加与滤波器状态承自原来的最后一帧)
// 必须恰好接在声明末端之后,与原帧的解码逐样本相同;尾裁落在流末尾则整体错位,
// 而总长度不变,只比长度发现不了。
func TestTrimAtDeclaredPositionWithExtraFrames(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	_, intact, err := decodeMode(t, bytes.NewReader(data), Strict())
	if err != nil {
		t.Fatal(err)
	}
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
	if want := (203*1152 - 1105 - 731) * 2; len(pcm) != want || d.Frames() != int64(want/2) {
		t.Fatalf("lenient delivered %d samples (Frames %d), want %d", len(pcm), d.Frames(), want)
	}
	if !slices.Equal(pcm[:toneDelivered*2], intact) {
		t.Fatal("output up to the declared end differs from the intact stream")
	}
	const spf = 1152
	requireSamplesEqual(t, "copied frames", pcm[(toneDelivered+spf)*2:(toneDelivered+10*spf)*2],
		decodeUntrimmed(t, data)[spf*2:10*spf*2], 2)
	if !slices.Equal(strictPCM, pcm) {
		t.Fatal("strict and lenient deliver different samples")
	}
	r := d.Report()
	if r.First == nil || r.First.Kind != IssueTruncated || r.DeclaredFrames != 193 || r.AudioFrames != 203 {
		t.Fatalf("report %+v first %+v", r, r.First)
	}
}

// 帧数少于声明:尾裁区间落在实际输出之外,不裁。
func TestTrimAtDeclaredPositionWithMissingFrames(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	starts, end := audioFrames(t, data)
	short := cat(data[:starts[len(starts)-2]], data[end:])
	d, pcm, err := decodeMode(t, bytes.NewReader(short), lenient())
	if err != nil {
		t.Fatalf("lenient: %v", err)
	}
	if want := (191*1152 - 1105) * 2; len(pcm) != want {
		t.Fatalf("delivered %d samples, want %d", len(pcm), want)
	}
	if r := d.Report(); r.First == nil || r.First.Kind != IssueTruncated || r.AudioFrames != 191 {
		t.Fatalf("report %+v first %+v", r, r.First)
	}
}

// 剪掉开头 5 帧:头裁仍从第一个解出的采样起算,尾裁区间落在输出之外。
func TestTrimWithLeadingFramesCut(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	starts, end := audioFrames(t, data)
	in := cat(data[:starts[0]], data[starts[5]:end])
	d, pcm, err := decodeMode(t, bytes.NewReader(in), lenient())
	if err != nil {
		t.Fatalf("lenient: %v", err)
	}
	if want := (188*1152 - 1105) * 2; len(pcm) != want {
		t.Fatalf("delivered %d samples, want %d", len(pcm), want)
	}
	if r := d.Report(); r.First == nil || r.First.Kind != IssueTruncated || r.AudioFrames != 188 {
		t.Fatalf("report %+v first %+v", r, r.First)
	}
}

// 最后一帧被截断、其后是 ID3v1:宽松模式丢弃不完整的帧,识别出尾部标签,不因帧数不符失败。
func TestLenientCutLastFrameBeforeID3v1(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	starts, _ := audioFrames(t, data)
	in := cat(data[:starts[len(starts)-1]+200], id3v1Tag())
	d, pcm, err := decodeMode(t, bytes.NewReader(in), lenient())
	if err != nil {
		t.Fatalf("lenient: %v", err)
	}
	if want := (192*1152 - 1105) * 2; len(pcm) != want {
		t.Fatalf("delivered %d samples, want %d", len(pcm), want)
	}
	if r := d.Report(); !r.Truncated || r.TrailingTags != TagID3v1 || r.AudioFrames != 192 {
		t.Fatalf("report %+v", r)
	}
}

// ICY 式数据块(长度字节 2,随后 32 字节元数据)插在帧边界或帧内:宽松模式跳过这 33 字节,
// 每一帧都解出。
func TestLenientICYBlock(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3")
	starts, end := audioFrames(t, data)
	icy := cat([]byte{2}, []byte("StreamTitle='x';"), make([]byte, 16))
	for name, at := range map[string]int{"frame boundary": starts[50], "inside a frame": starts[50] + 150} {
		t.Run(name, func(t *testing.T) {
			in := cat(data[:at], icy, data[at:end])
			d, pcm, err := decodeMode(t, bytes.NewReader(in), lenient())
			if err != nil {
				t.Fatalf("lenient: %v", err)
			}
			r := d.Report()
			if len(pcm) != toneDelivered*2 || r.AudioFrames != 193 || r.SkippedBytes != 33 || r.Resyncs != 1 || r.Clean() {
				t.Fatalf("delivered %d samples, report %+v", len(pcm), r)
			}
		})
	}
}
