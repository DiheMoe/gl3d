package gl3d

import (
	"bytes"
	"errors"
	"testing"
)

func TestReservoirAssemble(t *testing.T) {
	r := newReservoir(511)

	// 帧 1:自带 4 字节 main data,main_data_begin=0
	f1, err := r.assemble(0, []byte("AAAA"))
	if err != nil {
		t.Fatal(err)
	}
	if string(f1) != "AAAA" {
		t.Fatalf("f1=%q", f1)
	}
	r.advance([]byte("AAAA"))

	// 帧 2:main_data_begin=2 → 数据从帧 1 的末尾 2 字节开始,即 "AA"+"BBBB"
	f2, err := r.assemble(2, []byte("BBBB"))
	if err != nil {
		t.Fatal(err)
	}
	if string(f2) != "AABBBB" {
		t.Fatalf("f2=%q, want AABBBB", f2)
	}
	r.advance([]byte("BBBB"))

	// 帧 3:回指 8 字节(帧 1 与帧 2 的全部)
	f3, err := r.assemble(8, []byte("CC"))
	if err != nil {
		t.Fatal(err)
	}
	if string(f3) != "AAAABBBBCC" {
		t.Fatalf("f3=%q, want AAAABBBBCC", f3)
	}
}

func TestReservoirUnderflow(t *testing.T) {
	r := newReservoir(511)
	r.advance([]byte("AAAA"))
	if _, err := r.assemble(5, []byte("BB")); err == nil {
		t.Fatal("want underflow error")
	}
	var c *CorruptError
	if _, err := r.assemble(5, []byte("BB")); !errors.As(err, &c) {
		t.Fatalf("err=%v, want CorruptError", err)
	}
}

func TestReservoirCap(t *testing.T) {
	r := newReservoir(255) // MPEG-2/2.5 的 reservoir 上限(main_data_begin 为 8 位)
	r.advance(bytes.Repeat([]byte{1}, 300))
	if r.len() != 255 {
		t.Fatalf("len=%d, want 255(封顶)", r.len())
	}
	// 封顶后回指 255 仍合法,256 越界
	if _, err := r.assemble(255, []byte{2}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.assemble(256, []byte{2}); err == nil {
		t.Fatal("want underflow at 256")
	}
}
