package gl3d

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func id3v1Tag() []byte {
	tag := make([]byte, 128)
	copy(tag, "TAG")
	copy(tag[3:], "gl3d")
	return tag
}

// 表 0 及缺失 reservoir 的静音路径可以不读位,仍须拒绝超过窗口的总预算。宽松模式与
// side info 非法同样处理,也就是丢弃这一帧,后面的帧照常解码。
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
		d, _, err = decodeMode(t, bytes.NewReader(data))
		if err != nil || d.Frames() != 1152 {
			t.Fatalf("missing=%d lenient: delivered %d frames, err=%v; want 1152 and no error", missing, d.Frames(), err)
		}
		rep := d.Report()
		if f := rep.First; rep.DroppedFrames != 1 || f == nil || f.Kind != IssueCorrupt || f.Frame != 0 {
			t.Fatalf("missing=%d lenient: report %+v, first %+v; want frame 0 dropped as corrupt", missing, rep, f)
		}
	}
}

// drain 以严格模式解码到流结束并返回解码器。io.EOF 视为正常结束,其他错误原样返回。
func drain(t *testing.T, r io.Reader) (*Decoder, error) {
	t.Helper()
	d, err := NewDecoder(context.Background(), r, Strict())
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

// 文件中途改变 MPEG 版本、采样率或声道数属不支持的特性。切换前的完整帧照常交付。
// MPEG-1 与 MPEG-2 的采样率集合不相交,版本改变必然伴随采样率改变,故无仅改版本的用例。
func TestMidStreamFormatChangeUnsupported(t *testing.T) {
	cases := []struct {
		name string
		h    [4]byte
		size int
	}{
		{"mono", mkHeader(3, 1, 1, 9, 0, 0, 3, 0, 0), 417},
		{"mpeg2 22.05k", mkHeader(2, 1, 1, 9, 0, 0, 0, 0, 0), 261},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := repeatFrames(mkHeader(3, 1, 1, 9, 0, 0, 0, 0, 0), 417, 3)
			data = append(data, repeatFrames(tc.h, tc.size, 3)...)
			d, err := drain(t, bytes.NewReader(data))
			var u *UnsupportedError
			if !errors.As(err, &u) || u.Feature != "mid-stream format change" {
				t.Fatalf("err=%v, want UnsupportedError %q", err, "mid-stream format change")
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
			// scalefac_compress 4 + window_switching 1。改为未定义的表 4
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
