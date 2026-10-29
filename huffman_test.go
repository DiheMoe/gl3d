package gl3d

import (
	"testing"
)

func TestHuffmanRoundTripAllTables(t *testing.T) {
	for tab, entries := range huffBigTables {
		if entries == nil {
			continue
		}
		linbits := int(huffLinbits[tab])
		for _, e := range entries {
			var w bitWriter
			w.write(int(e.code), int(e.length))
			// 码字之后,每个值先写 linbits 扩展位、再写符号位(非零时);x 先于 y
			extX, extY := 0, 0
			maxExt := (1 << linbits) - 1
			if e.x == 15 && linbits > 0 {
				extX = 3 & maxExt
				w.write(extX, linbits)
			}
			if e.x != 0 {
				w.write(1, 1) // x 为负
			}
			if e.y == 15 && linbits > 0 {
				extY = 1 & maxExt
				w.write(extY, linbits)
			}
			if e.y != 0 {
				w.write(0, 1) // y 为正
			}
			br := &bitReader{data: w.bytes}
			x, y, err := huff.decodePair(br, tab)
			if err != nil {
				t.Fatalf("table %d entry %+v: %v", tab, e, err)
			}
			wantX := int(e.x) + extX
			if e.x != 0 {
				wantX = -wantX
			}
			if x != wantX || y != int(e.y)+extY {
				t.Fatalf("table %d entry %+v: got (%d,%d), want (%d,%d)", tab, e, x, y, wantX, int(e.y)+extY)
			}
		}
	}
}

func TestHuffmanQuadRoundTrip(t *testing.T) {
	for ti, entries := range [2][]quadEntry{huffQuadA, huffQuadB} {
		for _, e := range entries {
			var w bitWriter
			w.write(int(e.code), int(e.length))
			for i := 0; i < 4; i++ {
				if e.flags&(8>>i) != 0 {
					w.write((i+1)&1, 1) // 偶数位写 1(负)、奇数位写 0(正)
				}
			}
			br := &bitReader{data: w.bytes}
			q, complete := decodeQuad(br, ti == 1, w.nbits)
			if !complete {
				t.Fatalf("quad table %d entry %+v: incomplete", ti, e)
			}
			if br.pos != w.nbits {
				t.Fatalf("quad table %d consumed %d bits, want %d", ti, br.pos, w.nbits)
			}
			for i := 0; i < 4; i++ {
				want := 0
				if e.flags&(8>>i) != 0 {
					if i&1 == 0 {
						want = -1
					} else {
						want = 1
					}
				}
				if q[i] != want {
					t.Fatalf("quad table %d entry %+v: got %v, pos %d want %d", ti, e, q, i, want)
				}
			}
		}
	}
}

// 表 0 没有码字:所在区域谱值全为 0、不消耗任何位。编码器对零能量区域会
// 合法地选择表 0,必须按全零接受,不能当作非法表。
func TestHuffmanTable0IsZeroWithoutBits(t *testing.T) {
	br := &bitReader{data: []byte{0xFF, 0xFF}}
	for i := 0; i < 3; i++ {
		x, y, err := huff.decodePair(br, 0)
		if err != nil || x != 0 || y != 0 {
			t.Fatalf("pair %d: got (%d,%d,%v), want (0,0,nil)", i, x, y, err)
		}
	}
	if br.pos != 0 {
		t.Fatalf("table 0 consumed %d bits, want 0", br.pos)
	}
}

func TestHuffmanErrors(t *testing.T) {
	// 表 4 与 14 未定义(32 个表号中的空位)
	for _, tab := range []int{4, 14} {
		br := &bitReader{data: []byte{0xFF, 0xFF}}
		if _, _, err := huff.decodePair(br, tab); err == nil {
			t.Fatalf("want error for undefined table %d", tab)
		}
	}
	// 全部 big_values 表都是完备前缀码,任意位模式都能匹配某个码字,不存在
	// "非法码字"输入;非法 Huffman 数据只能靠位流耗尽或 part2_3_length 越界检出。
	br3 := &bitReader{data: []byte{0x0F}}
	if _, _, err := huff.decodePair(br3, 13); err == nil {
		t.Fatal("want error for bitstream underrun")
	}
	br4 := &bitReader{data: []byte{0x00}}
	if _, complete := decodeQuad(br4, false, 8); complete {
		t.Fatal("want incomplete quad at granule end")
	}
}

// count1 的码字与符号位必须作为整体落在 granule 内。不足的后缀属于
// stuffing,即使物理缓冲里已有下一 granule 的字节也不能越界消费。
func TestHuffmanQuadStopsAtGranuleBoundary(t *testing.T) {
	for ti, entries := range [2][]quadEntry{huffQuadA, huffQuadB} {
		for _, e := range entries {
			for offset := 0; offset < 8; offset++ {
				var w bitWriter
				w.write(0, offset)
				w.write(int(e.code), int(e.length))
				for i := 0; i < 4; i++ {
					if e.flags&(8>>i) != 0 {
						w.write(i&1, 1)
					}
				}
				for limit := offset; limit < w.nbits; limit++ {
					// 分别测试物理窗口恰好结束与后面仍有足量数据。
					for _, data := range [][]byte{w.bytes[:(limit+7)/8], append(append([]byte{}, w.bytes...), 0xA5, 0x5A)} {
						br := &bitReader{data: data, pos: offset}
						if _, complete := decodeQuad(br, ti == 1, limit); complete || br.pos != offset {
							t.Fatalf("table %d entry %+v offset %d limit %d: complete=%v pos=%d", ti, e, offset, limit, complete, br.pos)
						}
					}
				}
			}
		}
	}
}
