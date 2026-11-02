package gl3d

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func id3v1Tag() []byte {
	tag := make([]byte, 128)
	copy(tag, "TAG")
	copy(tag[3:], "gl3d")
	return tag
}

// 后续坏帧头不能使已完整收到的前一帧丢失,报错序号指向坏帧头。
func TestCompleteFramesBeforeBadHeader(t *testing.T) {
	data := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 3)
	data = append(data, "JUNK"...)
	d, err := drain(t, bytes.NewReader(data))
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) || corrupt.Frame != 3 {
		t.Fatalf("error=%v, want CorruptError at frame 3", err)
	}
	if d.Frames() != 3456 {
		t.Fatalf("delivered %d frames, want 3456", d.Frames())
	}
}

// 表 0 及缺失 reservoir 的静音路径可以不读位,仍须拒绝超过窗口的总预算。
func TestFrameRejectsImpossibleMainDataBudget(t *testing.T) {
	for _, missing := range []int{0, 511} {
		data := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 2)
		si := data[4:36]
		setBits(si, 0, 9, missing)
		for i := range 4 {
			setBits(si, 20+i*59, 12, 4095)
			setBits(si, 20+i*59+12, 9, 288)
		}
		d, err := drain(t, bytes.NewReader(data))
		var corrupt *CorruptError
		if !errors.As(err, &corrupt) || corrupt.Frame != 0 {
			t.Fatalf("missing=%d: error=%v, want CorruptError at frame 0", missing, err)
		}
		if d.Frames() != 0 || d.DecodedFrames() != 0 {
			t.Fatalf("missing=%d: accepted impossible main data", missing)
		}
	}
}

// drain 解码到流结束并返回解码器;io.EOF 视为正常结束,其他错误原样返回。
func drain(t *testing.T, r io.Reader) (*Decoder, error) {
	t.Helper()
	d, err := NewDecoder(context.Background(), r)
	if err != nil {
		return nil, err
	}
	buf := make([]float64, 4096)
	for {
		n, err := d.ReadPCM(context.Background(), buf)
		if err == io.EOF {
			return d, nil
		}
		if err != nil {
			return d, err
		}
		if n == 0 {
			t.Fatal("no progress: (0, nil)")
		}
	}
}

func TestID3v1TrailerSkipped(t *testing.T) {
	cases := []struct {
		file       string
		wantFrames int64
	}{
		{"sine440-stereo-44k1-cbr192.mp3", 220500},       // Info+LAME 标签:样本数按 Xing 帧数与 gapless 尾裁得出
		{"sine440-stereo-44k1-cbr192-shine.mp3", 220032}, // 无标签:不裁剪,191 个完整帧
	}
	for _, tc := range cases {
		plain := readCorpus(t, tc.file)
		tagged := append(append([]byte{}, plain...), id3v1Tag()...)
		_, want := decodeAll(t, bytes.NewReader(plain))
		readers := map[string]func() io.Reader{
			"bytes":   func() io.Reader { return bytes.NewReader(tagged) },
			"onebyte": func() io.Reader { return &oneByteReader{r: bytes.NewReader(tagged)} },
		}
		for rname, mk := range readers {
			t.Run(tc.file+"/"+rname, func(t *testing.T) {
				d, got := decodeAll(t, mk())
				if d.Frames() != tc.wantFrames {
					t.Fatalf("Frames()=%d, want %d", d.Frames(), tc.wantFrames)
				}
				if !bytes.Equal(float64Bytes(got), float64Bytes(want)) {
					t.Fatal("PCM differs from the untagged stream")
				}
			})
		}
	}
}

func TestID3v1TruncatedTag(t *testing.T) {
	data := append(readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3"), id3v1Tag()[:60]...)
	_, err := drain(t, bytes.NewReader(data))
	var tr *TruncatedError
	if !errors.As(err, &tr) {
		t.Fatalf("err=%v, want TruncatedError", err)
	}
}

// 流中途切换到已识别但不支持的格式(此处为 Layer II)须报不支持错误;切换前
// 已完整解码的 Layer III 帧照常交付,不能当作同步丢失。
func TestMidStreamLayerChangeUnsupported(t *testing.T) {
	data := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 3)
	data = append(data, repeatFrames(mkHeader(3, 2, 1, 10, 0, 0, 0, 0, 0), 626, 3)...)
	d, err := drain(t, bytes.NewReader(data))
	var u *UnsupportedError
	if !errors.As(err, &u) || !strings.Contains(u.Feature, "Layer II") {
		t.Fatalf("err=%v, want UnsupportedError mentioning Layer II", err)
	}
	if d.Frames() != 3*1152 {
		t.Fatalf("Frames()=%d, want %d (3 个 Layer III 帧)", d.Frames(), 3*1152)
	}
}

// 文件中途改变 MPEG 版本、采样率或声道数属不支持的特性;切换前的完整帧照常交付。
// MPEG-1 与 MPEG-2 的采样率集合不相交,版本改变必然伴随采样率改变,故无仅改版本的用例。
func TestMidStreamFormatChangeUnsupported(t *testing.T) {
	cases := []struct {
		name string
		h    [4]byte
		size int
	}{
		{"sample rate 48k", mkHeader(3, 1, 1, 9, 1, 0, 0, 0, 0), 384},
		{"mono", mkHeader(3, 1, 1, 9, 0, 0, 3, 0, 0), 417},
		{"mpeg2 22.05k", mkHeader(2, 1, 1, 9, 0, 0, 0, 0, 0), 261},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 3)
			data = append(data, repeatFrames(tc.h, tc.size, 3)...)
			d, err := drain(t, bytes.NewReader(data))
			var u *UnsupportedError
			if !errors.As(err, &u) || !strings.Contains(u.Feature, "format change") {
				t.Fatalf("err=%v, want UnsupportedError mentioning format change", err)
			}
			if d.Frames() != 3*1152 {
				t.Fatalf("Frames()=%d, want %d (切换前的 3 帧)", d.Frames(), 3*1152)
			}
		})
	}
}

// setBits 把 v 的低 n 位(MSB 优先)写入 b 的第 pos 位起。
func setBits(b []byte, pos, n, v int) {
	for i := 0; i < n; i++ {
		bit := byte(v>>(n-1-i)) & 1
		idx, shift := (pos+i)/8, 7-(pos+i)%8
		b[idx] = b[idx]&^(1<<shift) | bit<<shift
	}
}

// 谱数据解码出错时,CorruptError 必须携带出错音频帧的序号,供调用方定位损坏位置。
func TestCorruptErrorReportsFrameIndex(t *testing.T) {
	data := readCorpus(t, "sine440-stereo-44k1-cbr192.mp3") // MPEG-1 立体声,无 CRC
	size, footer, err := parseID3v2Header([10]byte(data[:10]))
	if err != nil {
		t.Fatal(err)
	}
	p := size
	if footer {
		p += 10
	}
	info, err := parseFrameHeader([4]byte(data[p : p+4]))
	if err != nil {
		t.Fatal(err)
	}
	p += info.frameSize // Info 帧不计入音频帧序号
	target := -1
	for idx := 0; p+4 <= len(data); idx++ {
		h, err := parseFrameHeader([4]byte(data[p : p+4]))
		if err != nil {
			t.Fatal(err)
		}
		var si sideInfo
		if err := parseSideInfo(&h, data[p+4:p+4+h.sideInfoSize], &si); err != nil {
			t.Fatal(err)
		}
		if gi := si.gr[0][0]; idx >= 10 && !gi.windowSwitching && gi.bigValues > 0 {
			// gr0/ch0 的 table_select[0] 位于 side info 第 54 位:main_data_begin 9 +
			// private 3 + scfsi 8 + part2_3_length 12 + big_values 9 + global_gain 8 +
			// scalefac_compress 4 + window_switching 1;改为未定义的表 4
			setBits(data[p+4:], 54, 5, 4)
			target = idx
			break
		}
		p += h.frameSize
	}
	if target < 0 {
		t.Fatal("no suitable frame to corrupt")
	}
	_, err = drain(t, bytes.NewReader(data))
	var c *CorruptError
	if !errors.As(err, &c) {
		t.Fatalf("err=%v, want CorruptError", err)
	}
	if c.Frame != int64(target) {
		t.Fatalf("CorruptError.Frame=%d, want %d (err=%v)", c.Frame, target, err)
	}
}

func TestID3v1FollowedByData(t *testing.T) {
	data := append(readCorpus(t, "sine440-stereo-44k1-cbr192-shine.mp3"), id3v1Tag()...)
	data = append(data, "trailing junk"...)
	_, err := drain(t, bytes.NewReader(data))
	var c *CorruptError
	if !errors.As(err, &c) {
		t.Fatalf("err=%v, want CorruptError", err)
	}
}
