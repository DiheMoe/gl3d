package gl3d

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"
)

// badInfoFrame 返回由语料的 Info 帧改出的、Xing 头声明的字段装不进帧的 Info 帧:码率降到
// 32 kbps(帧长 104 B),flags 声明全部四个字段(共需 120 字节)。
func badInfoFrame(t *testing.T) []byte {
	t.Helper()
	data := readCorpus(t, "tone10ms-stereo-44k1-cbr128-lame.mp3")
	start, xo, _ := infoFrame(t, data)
	frame := bytes.Clone(data[start:])
	frame[2] = frame[2]&0x0D | 1<<4 // 码率序号 1,清除 padding 位
	h, err := parseFrameHeader([4]byte(frame[:4]))
	if err != nil || h.frameSize != 104 {
		t.Fatalf("fixture: header %v, frame size %d, want 104", err, h.frameSize)
	}
	frame = frame[:h.frameSize]
	binary.BigEndian.PutUint32(frame[xo+4:], 0xF)
	magic, off := metaMagic(frame, h)
	if _, _, err := parseXing(frame[off:]); magic != "Info" || err == nil {
		t.Fatalf("fixture: magic %q, parseXing error %v; want an Info frame that does not parse", magic, err)
	}
	return frame
}

// 宽松模式的 NewDecoder 只在读取出错、取消、无音频、不支持的格式或超出限制时失败,不为
// 流本身的格式问题失败:开头的 ID3v2 头无效时按垃圾往后找帧;数据在帧之前结束时没有可
// 交出的音频;首帧的 Xing 标签解析不了时整帧丢弃,不采用其中的字段。严格模式的错误不变。
func TestLenientOpenFailsOnlyWithoutAnythingToDecode(t *testing.T) {
	shine := readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3")
	_, want := decodeAll(t, bytes.NewReader(shine))
	bad := badInfoFrame(t)
	sourceErr := errors.New("source reset")
	badID3 := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0x80} // size 的最后一个字节不是 syncsafe
	for _, tc := range []struct {
		name    string
		src     func() io.Reader
		strict  string // 严格模式 NewDecoder 的错误类别
		lenient string // 宽松模式的错误类别;"ok" 表示建成解码器并解出 shine 的全部样本
		first   Issue  // 宽松模式建成解码器时的第一处异常
	}{
		{"ID3v2 header with a non-syncsafe size",
			func() io.Reader { return bytes.NewReader(cat(badID3, shine)) },
			"corrupt", "ok", Issue{Kind: IssueCorrupt, Frame: -1, Offset: 0}},
		{"valid ID3v2 tag then an invalid header",
			func() io.Reader { return bytes.NewReader(cat(id3v2Tag(50), badID3, shine)) },
			"corrupt", "ok", Issue{Kind: IssueCorrupt, Frame: -1, Offset: 60}},
		{"invalid ID3v2 header and nothing after it",
			func() io.Reader { return bytes.NewReader(badID3) },
			"corrupt", "noaudio", Issue{}},
		{"ID3 and five bytes at the end of the stream",
			func() io.Reader { return bytes.NewReader([]byte("ID3\x04\x00\x00\x00\x00")) },
			"truncated", "noaudio", Issue{}},
		{"leading tag cut short",
			func() io.Reader { return bytes.NewReader(id3v2Tag(200)[:100]) },
			"truncated", "noaudio", Issue{}},
		{"junk ending in 0xFF at the end of the stream",
			func() io.Reader { return bytes.NewReader(cat(noSync(100), []byte{0xFF})) },
			"truncated", "noaudio", Issue{}},
		{"junk ending in a half frame header at the end of the stream",
			func() io.Reader { return bytes.NewReader(cat(noSync(100), []byte{0xFF, 0xFB})) },
			"truncated", "noaudio", Issue{}},
		{"malformed Xing first frame then frames",
			func() io.Reader { return bytes.NewReader(cat(bad, shine)) },
			"corrupt", "ok", Issue{Kind: IssueCorrupt, Frame: 0, Offset: 0}},
		// 源出错时未经确认交出的首帧带着解析不了的标签,多半是伪同步:报告源错误
		{"unconfirmed malformed Xing first frame and a source error",
			func() io.Reader { return &dataWithErrReader{r: bytes.NewReader(bad), err: sourceErr} },
			"other: source reset", "other: source reset", Issue{}},
	} {
		t.Run(tc.name+"/strict", func(t *testing.T) {
			d, _, err := decodeMode(t, tc.src(), Strict())
			if d != nil || errClass(err) != tc.strict {
				t.Fatalf("decoder=%t err=%v, want no decoder and a %s error", d != nil, err, tc.strict)
			}
		})
		t.Run(tc.name+"/lenient", func(t *testing.T) {
			d, got, err := decodeMode(t, tc.src(), lenient())
			if tc.lenient != "ok" {
				if d != nil || errClass(err) != tc.lenient {
					t.Fatalf("decoder=%t err=%v, want no decoder and a %s error", d != nil, err, tc.lenient)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("%d samples, want the %d of the frames after the damage", len(got), len(want))
			}
			if r := d.Report(); r.First == nil || *r.First != tc.first {
				t.Fatalf("report %+v, first %+v, want first %+v", r, r.First, tc.first)
			}
		})
	}
}
