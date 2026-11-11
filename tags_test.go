package gl3d

import (
	"testing"
)

// id3v2HeaderAt 是 parseID3v2Header 的不分配内存的预检。主版本号、修订号与 size 的每个
// 字节取遍 0–255,两者接受的范围一致。
func TestID3v2HeaderAtAgreesWithParse(t *testing.T) {
	for _, pos := range []int{3, 4, 6, 7, 8, 9} {
		for v := 0; v < 256; v++ {
			b := [10]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}
			b[pos] = byte(v)
			_, _, err := parseID3v2Header(b)
			if got := id3v2HeaderAt(b[:]); got != (err == nil) {
				t.Fatalf("byte %d = %#x: id3v2HeaderAt=%v, parseID3v2Header error=%v", pos, v, got, err)
			}
			want := v < 0x80 // size 是 syncsafe
			if pos <= 4 {
				want = v != 0xFF // 规范保证主版本号与修订号不会是 0xFF
			}
			if (err == nil) != want {
				t.Fatalf("byte %d = %#x: parseID3v2Header error=%v, want valid=%v", pos, v, err, want)
			}
		}
	}
	if id3v2HeaderAt([]byte("ID3\x04\x00\x00\x00\x00\x00")) || id3v2HeaderAt([]byte("TAG\x04\x00\x00\x00\x00\x00\x00")) {
		t.Fatal("id3v2HeaderAt accepted a short buffer or another identifier")
	}
}

// 主版本只能是 2、3、4,标志位只能用该版本定义的那些。v2.2 定义不同步与压缩,v2.3 加上
// 扩展头与实验性,v2.4 再加上 footer。
func TestID3v2HeaderFlaw(t *testing.T) {
	for _, tc := range []struct {
		major, revision, flags byte
		flaw                   bool
	}{
		{2, 0, 0xC0, false},
		{3, 1, 0xE0, false},
		{4, 0, 0xF0, false},
		{2, 0, 0x20, true},
		{3, 0, 0x10, true},
		{4, 0, 0x08, true},
		{4, 0, 0x01, true},
		{0, 0, 0, true},
		{1, 0, 0, true},
		{5, 0, 0, true},
	} {
		err := id3v2HeaderFlaw([10]byte{'I', 'D', '3', tc.major, tc.revision, tc.flags, 0, 0, 0, 0})
		if (err != nil) != tc.flaw {
			t.Errorf("ID3v2.%d.%d flags %#02x: error %v, want flaw=%v", tc.major, tc.revision, tc.flags, err, tc.flaw)
		}
	}
}
