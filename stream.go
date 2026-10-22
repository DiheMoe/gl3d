package gl3d

import (
	"context"
	"fmt"
	"io"
)

// streamBufSize 是输入缓冲上限,与同步搜索预算一致;正常解码路径只需容纳
// 一个最大帧(≤1441 B)加确认下一帧同步位的 4 B。
const streamBufSize = 64 << 10

// stream 在调用方的 io.Reader 上提供有界缓冲,正确处理该接口的边角行为:
// 短读、数据与 EOF 或错误同时返回(数据先用,错误等缓冲耗尽后再上报)、
// 以及连续无进展读取。
type stream struct {
	r    io.Reader
	back []byte // 固定容量的底层缓冲
	buf  []byte // 未消费字节,始终是 back 的子切片;drop 只前移起点
	off  int64  // buf[0] 在流中的绝对偏移
	eof  bool   // 底层已返回 EOF
	err  error  // 黏性非 EOF 源错误
}

func newStream(r io.Reader) *stream {
	back := make([]byte, streamBufSize)
	return &stream{r: r, back: back, buf: back[:0]}
}

func (s *stream) tell() int64 { return s.off }

// fill 累积缓冲到至少 n 字节。返回 nil 不保证凑满 n(EOF 时凑不满),调用方
// 须检查 len(s.buf);取消与源错误只在缓冲不足以满足 n 时向上返回。
func (s *stream) fill(ctx context.Context, n int) error {
	if n > len(s.back) {
		return fmt.Errorf("gl3d: internal: buffer request %d exceeds capacity %d", n, len(s.back))
	}
	if len(s.buf) < n && cap(s.buf) < n {
		// 起点之后的空间凑不满 n:把未消费字节搬回缓冲起点
		s.buf = s.back[:copy(s.back, s.buf)]
	}
	idle := 0
	for len(s.buf) < n && !s.eof && s.err == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := s.r.Read(s.buf[len(s.buf):cap(s.buf)])
		if m > 0 {
			s.buf = s.buf[:len(s.buf)+m]
			idle = 0
		} else if err == nil {
			idle++
			if idle >= 100 {
				s.err = io.ErrNoProgress
			}
		}
		if err == io.EOF {
			s.eof = true
		} else if err != nil {
			s.err = err
		}
	}
	if len(s.buf) < n && s.err != nil {
		return s.err
	}
	return nil
}

func (s *stream) drop(n int) {
	s.off += int64(n)
	s.buf = s.buf[n:]
}

// skip 分块丢弃接下来的 n 字节,内存占用与 n 无关;流在跳完之前结束则返回
// false(其余错误为取消或源读取错误)。
func (s *stream) skip(ctx context.Context, n int64) (bool, error) {
	for n > 0 {
		chunk := int(min(n, int64(len(s.back))))
		if err := s.fill(ctx, chunk); err != nil {
			return false, err
		}
		if len(s.buf) == 0 {
			return false, nil
		}
		m := min(chunk, len(s.buf))
		s.drop(m)
		n -= int64(m)
	}
	return true, nil
}
