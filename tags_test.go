package gl3d

import (
	"testing"
)

// id3v2HeaderAt 是 parseID3v2Header 的不分配内存的预检:size 的每个字节取遍 0–255,两者
// 接受的范围一致。
func TestID3v2HeaderAtAgreesWithParse(t *testing.T) {
	for pos := 6; pos < 10; pos++ {
		for v := 0; v < 256; v++ {
			b := [10]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}
			b[pos] = byte(v)
			_, _, err := parseID3v2Header(b)
			if got := id3v2HeaderAt(b[:]); got != (err == nil) {
				t.Fatalf("size byte %d = %#x: id3v2HeaderAt=%v, parseID3v2Header error=%v", pos-6, v, got, err)
			}
		}
	}
	if id3v2HeaderAt([]byte("ID3\x04\x00\x00\x00\x00\x00")) || id3v2HeaderAt([]byte("TAG\x04\x00\x00\x00\x00\x00\x00")) {
		t.Fatal("id3v2HeaderAt accepted a short buffer or another identifier")
	}
}
