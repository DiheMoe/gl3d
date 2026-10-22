package gl3d

import (
	"testing"
)

func TestParseID3v2Header(t *testing.T) {
	size, footer, err := parseID3v2Header([10]byte{'I', 'D', '3', 4, 0, 0x10, 0, 0, 0, 34})
	if err != nil {
		t.Fatal(err)
	}
	if size != 44 || !footer {
		t.Fatalf("size=%d footer=%v, want 44/true", size, footer)
	}
	if _, _, err := parseID3v2Header([10]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0x80, 0}); err == nil {
		t.Fatal("want error for non-syncsafe size byte")
	}
}

func TestParseXingAndLAME(t *testing.T) {
	var b []byte
	b = append(b, "Xing"...)
	b = append(b, 0, 0, 0, 0x0F)
	b = append(b, 0, 0, 0, 193)
	b = append(b, 0, 1, 0xDB, 0x19)
	b = append(b, make([]byte, 100)...)
	b = append(b, 0, 0, 0, 0)
	lame := make([]byte, 36)
	copy(lame, "LAME3.100")
	lame[9] = 0x04 // vbr method mtrh
	// 字节 21–23:enc_delay 与 enc_padding 各 12 bit,此处为 576 与 1260
	lame[21] = 0x24
	lame[22] = 0x04
	lame[23] = 0xEC
	b = append(b, lame...)

	x, off, err := parseXing(b)
	if err != nil {
		t.Fatal(err)
	}
	if off != 120 {
		t.Fatalf("lame offset=%d, want 120", off)
	}
	if x.kind != "xing" || x.frames == nil || *x.frames != 193 || x.bytes == nil || *x.bytes != 121625 || !x.toc {
		t.Fatalf("xing: %+v", x)
	}
	l, ok := parseLAMETag(b[off:])
	if !ok {
		t.Fatal("want LAME tag")
	}
	if l.encoder != "LAME3.100" || l.vbrMethod != 4 || l.encDelay != 576 || l.encPadding != 1260 {
		t.Fatalf("lame: %+v", l)
	}
}

func TestParseXingRejectsTruncated(t *testing.T) {
	b := []byte("Info")
	b = append(b, 0, 0, 0, 1) // flags: frames present
	b = append(b, 0, 0)
	if _, _, err := parseXing(b); err == nil {
		t.Fatal("want error for truncated xing fields")
	}
}

func TestParseLAMETagAbsent(t *testing.T) {
	if _, ok := parseLAMETag(make([]byte, 10)); ok {
		t.Fatal("too short, want absent")
	}
	bad := make([]byte, 36)
	bad[0] = 0xFF
	if _, ok := parseLAMETag(bad); ok {
		t.Fatal("non-printable encoder, want absent")
	}
}

func TestParseVBRI(t *testing.T) {
	var b []byte
	b = append(b, "VBRI"...)
	b = append(b, 0, 1)             // version
	b = append(b, 0, 0)             // delay
	b = append(b, 0, 50)            // quality
	b = append(b, 0, 1, 0xDB, 0x19) // bytes
	b = append(b, 0, 0, 0, 193)     // frames
	b = append(b, 0, 100)           // toc_entries
	b = append(b, 0, 1)             // toc_scale
	b = append(b, 0, 2)             // toc_entry_bytes
	b = append(b, 0, 15)            // toc_entry_frames
	v, err := parseVBRI(b)
	if err != nil {
		t.Fatal(err)
	}
	if v.frames != 193 || v.bytes != 121625 {
		t.Fatalf("vbri: %+v", v)
	}
	if _, err := parseVBRI([]byte("VBRI...too short")); err == nil {
		t.Fatal("want error for truncated VBRI")
	}
}
