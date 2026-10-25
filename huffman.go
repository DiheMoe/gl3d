package gl3d

import "fmt"

// Huffman 解码。码表(huffman_tables.go)由 tools/huffgen 从 minimp3.h(CC0)
// 提取的协议事实生成,均为完备前缀码。

// huffDecoder 把码表展开为前缀查表:一级表以接下来 8 位为下标,码长 ≤ 8 的码字
// 在一级表直接命中;更长的码字按 8 位前缀分组,每组一张以剩余位为下标的二级表。
// 查表结构在 init 时由 huffman_tables.go 的码字列表推导,不含新的协议数据。
type huffDecoder struct {
	pair  [32]*huffFast
	quadA [64]uint16 // 以 6 位为下标:码长<<8 | 标志位(vwxy)
	quadB [16]uint16 // 以 4 位为下标
}

// huffFast 是一张 big_values 表的两级查表。表项:叶子为 码长<<8 | x<<4 | y;
// 子表指针为 1<<31 | 子表位数<<24 | 子表在 l2 中的偏移;0 表示无此码字。
type huffFast struct {
	l1 [256]uint32
	l2 []uint32
}

const huffSubFlag = 1 << 31

var huff = newHuffDecoder()

func newHuffDecoder() *huffDecoder {
	d := &huffDecoder{}
	// 16..23 与 24..31 各自共享一套只读码字,仅 huffLinbits 按表号变化。
	for t, entries := range huffBigTables {
		switch {
		case t >= 17 && t <= 23:
			d.pair[t] = d.pair[16]
		case t >= 25 && t <= 31:
			d.pair[t] = d.pair[24]
		case entries != nil:
			d.pair[t] = buildHuffFast(entries)
		}
	}
	for _, e := range huffQuadA {
		fillPrefix(d.quadA[:], 6, uint32(e.code), int(e.length), uint16(e.length)<<8|uint16(e.flags))
	}
	for _, e := range huffQuadB {
		fillPrefix(d.quadB[:], 4, uint32(e.code), int(e.length), uint16(e.length)<<8|uint16(e.flags))
	}
	return d
}

// fillPrefix 在以 width 位为下标的表中填入码字 code(长 n ≤ width)的全部扩展。
func fillPrefix[T uint16 | uint32](tab []T, width int, code uint32, n int, v T) {
	base := code << (width - n)
	for i := uint32(0); i < 1<<(width-n); i++ {
		tab[base+i] = v
	}
}

func buildHuffFast(entries []huffEntry) *huffFast {
	f := &huffFast{}
	// 码长超过 8 的码字按 8 位前缀分组,子表位数取组内最长码的剩余位数
	var subBits [256]int
	for _, e := range entries {
		if n := int(e.length); n > 8 {
			p := e.code >> (n - 8)
			subBits[p] = max(subBits[p], n-8)
		}
	}
	for p, nb := range subBits {
		if nb > 0 {
			f.l1[p] = huffSubFlag | uint32(nb)<<24 | uint32(len(f.l2))
			f.l2 = append(f.l2, make([]uint32, 1<<nb)...)
		}
	}
	for _, e := range entries {
		n := int(e.length)
		leaf := uint32(n)<<8 | uint32(e.x)<<4 | uint32(e.y)
		if n <= 8 {
			fillPrefix(f.l1[:], 8, e.code, n, leaf)
			continue
		}
		sub := f.l1[e.code>>(n-8)]
		nb := int(sub >> 24 & 15)
		off := int(sub & 0xFFFFFF)
		fillPrefix(f.l2[off:off+1<<nb], nb, e.code&(1<<(n-8)-1), n-8, leaf)
	}
	return f
}

// decodePair 解码 big_values 区的一对谱值,含 linbits 扩展(值为 15 时)与
// 非零值的符号位(0=正 1=负)。表 0 没有码字:区域内谱值全为 0、不读位。
func (d *huffDecoder) decodePair(br *bitReader, table int) (int, int, error) {
	if table == 0 {
		return 0, 0, nil
	}
	f := d.pair[table]
	if f == nil {
		return 0, 0, &CorruptError{Frame: -1, Detail: "invalid huffman table number"}
	}
	bits := br.peek32()
	e := f.l1[bits>>24]
	if e&huffSubFlag != 0 {
		nb := e >> 24 & 15
		e = f.l2[e&0xFFFFFF+bits<<8>>(32-nb)]
	}
	n := int(e >> 8 & 31)
	if n == 0 {
		return 0, 0, &CorruptError{Frame: -1, Detail: fmt.Sprintf("invalid huffman codeword in table %d at bit %d", table, br.pos)}
	}
	if br.pos+n > len(br.data)*8 {
		return 0, 0, &CorruptError{Frame: -1, Detail: "huffman data underrun"}
	}
	br.pos += n
	linbits := int(huffLinbits[table])
	x, err := d.extend(br, int(e>>4&15), linbits)
	if err != nil {
		return 0, 0, err
	}
	y, err := d.extend(br, int(e&15), linbits)
	if err != nil {
		return 0, 0, err
	}
	return x, y, nil
}

func (d *huffDecoder) extend(br *bitReader, v, linbits int) (int, error) {
	if v == 15 && linbits > 0 {
		ext, err := br.read(linbits)
		if err != nil {
			return 0, &CorruptError{Frame: -1, Detail: "huffman linbits underrun"}
		}
		v += ext
	}
	if v == 0 {
		return 0, nil
	}
	sign, err := br.read(1)
	if err != nil {
		return 0, &CorruptError{Frame: -1, Detail: "huffman sign bit underrun"}
	}
	if sign != 0 {
		v = -v
	}
	return v, nil
}

// decodeQuad 解码 count1 区的一个四元组(count1table_select 选择表 A/B)。
func decodeQuad(br *bitReader, tableB bool) ([4]int, error) {
	bits := br.peek32()
	var e uint16
	if tableB {
		e = huff.quadB[bits>>28]
	} else {
		e = huff.quadA[bits>>26]
	}
	n := int(e >> 8)
	if n == 0 {
		return [4]int{}, &CorruptError{Frame: -1, Detail: "invalid count1 codeword"}
	}
	if br.pos+n > len(br.data)*8 {
		return [4]int{}, &CorruptError{Frame: -1, Detail: "count1 data underrun"}
	}
	br.pos += n
	var out [4]int
	for i := 0; i < 4; i++ {
		if e&(8>>i) == 0 {
			continue
		}
		sign, err := br.read(1)
		if err != nil {
			return [4]int{}, &CorruptError{Frame: -1, Detail: "count1 sign bit underrun"}
		}
		if sign != 0 {
			out[i] = -1
		} else {
			out[i] = 1
		}
	}
	return out, nil
}
