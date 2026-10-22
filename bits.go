package gl3d

import (
	"encoding/binary"
	"fmt"
)

// bitReader 以 MSB 优先从字节切片读位(MPEG 音频位流的位序约定)。
type bitReader struct {
	data []byte
	pos  int // 位位置
}

func (b *bitReader) read(n int) (int, error) {
	if n < 0 || n > 32 {
		return 0, fmt.Errorf("gl3d: internal: bad bit read width %d", n)
	}
	if b.pos+n > len(b.data)*8 {
		return 0, fmt.Errorf("gl3d: bitstream underrun at bit %d, want %d more", b.pos, n)
	}
	if n == 0 {
		return 0, nil
	}
	v := int(b.peek32() >> (32 - n))
	b.pos += n
	return v, nil
}

// peek32 返回从当前位置起的 32 位(MSB 对齐),越过数据末尾的位补 0;不移动位置。
func (b *bitReader) peek32() uint32 {
	i := b.pos >> 3
	var w uint64
	if i+8 <= len(b.data) {
		w = binary.BigEndian.Uint64(b.data[i:])
	} else {
		for j := i; j < i+8; j++ {
			w <<= 8
			if j < len(b.data) {
				w |= uint64(b.data[j])
			}
		}
	}
	return uint32(w << (b.pos & 7) >> 32)
}

func (b *bitReader) bitsLeft() int { return len(b.data)*8 - b.pos }
