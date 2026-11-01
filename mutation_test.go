package gl3d

import (
	"fmt"
	"testing"
)

// mutateStream 按种子对 data 做一次变异并返回新切片,不改动也不引用 data:
// 翻转一个字节、删除、插入、截断、改写五种操作由 r%5 选择,位置与长度取自 r 之后的
// splitmix64 序列。
func mutateStream(data []byte, seed, i uint64) []byte {
	r := splitmix64(seed<<16 | i)
	kind := r % 5
	next := func() uint64 { r = splitmix64(r); return r }
	random := func(n int) []byte {
		b := make([]byte, n)
		for k := range b {
			b[k] = byte(next())
		}
		return b
	}
	switch kind {
	case 0:
		out := append([]byte(nil), data...)
		out[next()%uint64(len(out))] ^= byte(1 + next()%255) // 异或非零值,保证字节确实改变
		return out
	case 1:
		n := int(1 + next()%64)
		pos := int(next() % uint64(len(data)-n+1))
		return append(append([]byte(nil), data[:pos]...), data[pos+n:]...)
	case 2:
		n := int(1 + next()%64)
		pos := int(next() % uint64(len(data)+1))
		out := append([]byte(nil), data[:pos]...)
		out = append(out, random(n)...)
		return append(out, data[pos:]...)
	case 3:
		return append([]byte(nil), data[:next()%uint64(len(data))]...)
	default:
		n := int(1 + next()%32)
		pos := int(next() % uint64(len(data)-n+1))
		out := append([]byte(nil), data...)
		copy(out[pos:], random(n))
		return out
	}
}

// 由种子决定的变异输入逐个经过 streamOnce:两种模式、整体与分块读取的结果一致,模式等价
// 成立,宽松模式不以截断或损坏结束,输出有限。开发机不跑 -fuzz,这是普通 go test 中的冒烟。
func TestStreamMutationSmoke(t *testing.T) {
	perFile := 100
	if testing.Short() {
		perFile = 20
	}
	for k, name := range []string{
		"sine440-stereo-44k1-cbr192.mp3",
		"pluck-stereo-32k-cbr96-crc-lame.mp3",
		"pluck-stereo-24k-cbr64-lame.mp3",
		"mixed-mono-8k-cbr64-built.mp3",
	} {
		// 取前 8 KiB 并收到最后一个完整帧的末尾:截在帧中间则每个输入都以截断结束。
		data := readCorpus(t, name)
		starts, end := audioFrames(t, data[:min(8<<10, len(data))])
		// 去掉 Xing/Info/VBRI 标签帧(保留其前的 ID3v2):它声明的帧数多于截短后的输入,未变异的输入因此不干净。
		tagEnd := id3v2Len(t, data)
		data = append(append([]byte(nil), data[:tagEnd]...), data[starts[0]:end]...)
		seed := uint64(k + 1)
		for i := 0; i < perFile; i++ {
			mutated := mutateStream(data, seed, uint64(i))
			t.Run(fmt.Sprintf("%s/%d", name, i), func(t *testing.T) {
				withLimits(t, func() { streamOnce(t, mutated) })
			})
		}
	}
}
