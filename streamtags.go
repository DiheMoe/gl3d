package gl3d

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
)

// 音频帧之间与之后的标签:ID3v1、TAG+、APEv2(含 v1)、Lyrics3 v1/v2 与 ID3v2。
// 每种标签由起点的标识识别,按自身的长度字段或结束标记确定范围,分块跳过,
// 内存占用与标签长度无关。

const (
	apeTagSize   = 32   // APEv2 头部与 footer 的长度
	tagPlusSize  = 227  // TAG+ 扩展标签的长度,其后紧接 ID3v1
	lyrics3v1Max = 5100 // Lyrics3 v1 正文的长度上限

	// tagProbeSize 是识别标签起点要看的字节数。只有 footer 的 APEv2 要看到第一个
	// 条目的完整键名(8 字节条目头、至多 255 字节键名与 NUL)
	tagProbeSize = 8 + 255 + 1

	apeIsHeader     = 1 << 29    // APEv2 标志:这是头部而不是 footer
	apeItemReserved = 0x1FFFFFF8 // APEv2 条目标志中必须为零的位(3–28)
)

// 编译期检查:探测范围须看到 TAG+ 之后 ID3v1 的标识,TAG+ 才能与标题以 + 开头的 ID3v1 区分
const _ = uint(tagProbeSize - tagPlusSize - 3)

var (
	id3Magic    = []byte("ID3")
	apeMagic    = []byte("APETAGEX")
	lyricsBegin = []byte("LYRICSBEGIN")
	lyricsEnd   = []byte("LYRICSEND")
)

// tagAt 识别 b 起点的标签并返回其种类,不是标签起点时返回 0。b 应含
// tagProbeSize 字节,流尾不足时为剩余的全部。
func tagAt(b []byte) TagSet {
	if kind := magicTagAt(b); kind != 0 {
		return kind
	}
	// 标识认得、头却无效时,其后的字节不是 APEv2 条目,否则 "ID3" 之后碰巧像条目的数据
	// 会被当成标签一路跳过
	if bytes.HasPrefix(b, id3Magic) || bytes.HasPrefix(b, apeMagic) {
		return 0
	}
	if apeItemAt(b) > 0 {
		return TagAPEv2
	}
	return 0
}

// magicTagAt 识别 b 起点带标识的标签:tagAt 识别的种类中,除只有 footer 的 APEv2
// 以外的全部。重新同步扫描垃圾时只认这些,因为 APEv2 条目没有标识,逐字节试探会误认。
func magicTagAt(b []byte) TagSet {
	switch {
	case len(b) >= tagPlusSize+3 && string(b[:4]) == "TAG+" && isID3v1Start(b[tagPlusSize:]):
		return TagID3v1Ext
	case isID3v1Start(b):
		return TagID3v1
	case id3v2HeaderAt(b):
		return TagID3v2
	case bytes.HasPrefix(b, apeMagic):
		if _, ok := parseAPEHeader(b); ok {
			return TagAPEv2
		}
	case bytes.HasPrefix(b, lyricsBegin):
		return TagLyrics3
	}
	return 0
}

// apeItemAt 报告 b 起点是否像一个 APEv2 条目:4 字节值长度、4 字节标志(保留位
// 为零)、2–255 个可打印 ASCII 字符的键名与结尾的 NUL。像则返回条目头、键名与
// NUL 的总长,否则返回 0。只有 footer 的 APEv2 标签从第一个条目开始。
func apeItemAt(b []byte) int {
	if len(b) < 8 || binary.LittleEndian.Uint32(b[4:])&apeItemReserved != 0 {
		return 0
	}
	for i := 8; i < len(b) && i <= 8+255; i++ {
		switch c := b[i]; {
		case c == 0:
			if i-8 < 2 {
				return 0
			}
			return i + 1
		case c < 0x20 || c > 0x7E:
			return 0
		}
	}
	return 0
}

// apeHeader 是 APEv2 头部或 footer 的字段。size 含条目与 footer,不含头部。
type apeHeader struct {
	size, items, flags uint32
}

// parseAPEHeader 解析 b 起点的 APEv2 头部或 footer。版本须为 1000(APEv1)或 2000。
func parseAPEHeader(b []byte) (apeHeader, bool) {
	if len(b) < apeTagSize || !bytes.HasPrefix(b, apeMagic) {
		return apeHeader{}, false
	}
	if v := binary.LittleEndian.Uint32(b[8:]); v != 1000 && v != 2000 {
		return apeHeader{}, false
	}
	return apeHeader{
		size:  binary.LittleEndian.Uint32(b[12:]),
		items: binary.LittleEndian.Uint32(b[16:]),
		flags: binary.LittleEndian.Uint32(b[20:]),
	}, true
}

// skipTag 跳过 buf 起点处 kind 类的标签,其起点已由 tagAt 识别。范围已知的标签
// 被流结束截断时报截断,内容与结构不符报格式错误。
func (f *framer) skipTag(ctx context.Context, kind TagSet) error {
	at := f.s.tell()
	switch kind {
	case TagID3v1:
		return f.skipSpan(ctx, id3v1Size, at, "ID3v1 tag")
	case TagID3v1Ext:
		return f.skipSpan(ctx, tagPlusSize, at, "TAG+ tag")
	case TagID3v2:
		size, footer, _ := parseID3v2Header([10]byte(f.s.buf[:10]))
		if footer {
			size += 10
		}
		return f.skipSpan(ctx, int64(size), at, "ID3v2 tag")
	case TagAPEv2:
		return f.skipAPEv2(ctx, at)
	case TagLyrics3:
		return f.skipLyrics3(ctx, at)
	}
	return fmt.Errorf("gl3d: internal: unknown tag kind %d", kind)
}

// skipSpan 跳过接下来属于 at 处标签的 n 字节。
func (f *framer) skipSpan(ctx context.Context, n, at int64, what string) error {
	if err := f.chargeTag(n, at); err != nil {
		return err
	}
	ok, err := f.s.skip(ctx, n)
	if err != nil {
		return err
	}
	if !ok {
		return &TruncatedError{Offset: at, What: what}
	}
	return nil
}

// skipAPEv2 跳过 APEv2 标签(也接受 v1)。带头部的标签按头部的长度跳到 footer
// 并核对。只有 footer 的标签逐个跳过条目,到 footer 处核对条目数与总长。
func (f *framer) skipAPEv2(ctx context.Context, at int64) error {
	if h, ok := parseAPEHeader(f.s.buf); ok {
		switch {
		case h.flags&apeIsHeader == 0:
			// 起点就是 footer,这样的标签只能是没有条目的空标签
			if h.size != apeTagSize || h.items != 0 {
				return &CorruptError{Frame: f.frames, Detail: "malformed APEv2 tag"}
			}
			return f.skipSpan(ctx, apeTagSize, at, "APEv2 tag")
		case h.size < apeTagSize:
			return &CorruptError{Frame: f.frames, Detail: "malformed APEv2 tag"}
		}
		// 头部与条目共 size 字节
		if err := f.skipSpan(ctx, int64(h.size), at, "APEv2 tag"); err != nil {
			return err
		}
		return f.apeFooter(ctx, at, int64(h.size), h.items)
	}
	var n int64 // 已跳过的条目字节数
	var items uint32
	for {
		if err := f.s.fill(ctx, tagProbeSize); err != nil {
			return err
		}
		if bytes.HasPrefix(f.s.buf, apeMagic) {
			return f.apeFooter(ctx, at, n+apeTagSize, items)
		}
		k := apeItemAt(f.s.buf)
		if k == 0 {
			if len(f.s.buf) == 0 {
				return &TruncatedError{Offset: at, What: "APEv2 tag"}
			}
			return &CorruptError{Frame: f.frames, Detail: "malformed APEv2 tag"}
		}
		item := int64(k) + int64(binary.LittleEndian.Uint32(f.s.buf))
		if err := f.skipSpan(ctx, item, at, "APEv2 tag"); err != nil {
			return err
		}
		n += item
		items++
	}
}

// apeFooter 核对并跳过 APEv2 footer。footer 给出的总长(含 footer、不含头部)与条目数
// 须与头部或逐个数出的结果一致。
func (f *framer) apeFooter(ctx context.Context, at, size int64, items uint32) error {
	if err := f.s.fill(ctx, apeTagSize); err != nil {
		return err
	}
	if len(f.s.buf) < apeTagSize {
		return &TruncatedError{Offset: at, What: "APEv2 tag"}
	}
	ft, ok := parseAPEHeader(f.s.buf)
	if !ok || ft.flags&apeIsHeader != 0 || int64(ft.size) != size || ft.items != items {
		return &CorruptError{Frame: f.frames, Detail: "malformed APEv2 tag"}
	}
	return f.skipSpan(ctx, apeTagSize, at, "APEv2 tag")
}

// skipLyrics3 跳过 Lyrics3 标签。v2 是 LYRICSBEGIN 之后的一串字段(3 个大写字母的
// 标识加 5 位十进制长度),以 6 位十进制总长(含 LYRICSBEGIN,不含其自身与结束
// 标记)与 LYRICS200 结束。v1 是至多 5100 字节的正文,以 LYRICSEND 结束。
func (f *framer) skipLyrics3(ctx context.Context, at int64) error {
	if err := f.s.fill(ctx, len(lyricsBegin)+8); err != nil {
		return err
	}
	if !lyricsField(f.s.buf[len(lyricsBegin):]) {
		return f.skipLyrics3v1(ctx, at)
	}
	n := int64(len(lyricsBegin))
	if err := f.skipSpan(ctx, n, at, "Lyrics3 tag"); err != nil {
		return err
	}
	for {
		if err := f.s.fill(ctx, 15); err != nil {
			return err
		}
		b := f.s.buf
		if lyricsField(b) {
			field := 8 + decimal(b[3:8])
			if err := f.skipSpan(ctx, field, at, "Lyrics3 tag"); err != nil {
				return err
			}
			n += field
			continue
		}
		if len(b) < 15 {
			return &TruncatedError{Offset: at, What: "Lyrics3 tag"}
		}
		if !isDigits(b[:6]) || string(b[6:15]) != "LYRICS200" || decimal(b[:6]) != n {
			return &CorruptError{Frame: f.frames, Detail: "malformed Lyrics3 tag"}
		}
		return f.skipSpan(ctx, 15, at, "Lyrics3 tag")
	}
}

func (f *framer) skipLyrics3v1(ctx context.Context, at int64) error {
	span := len(lyricsBegin) + lyrics3v1Max + len(lyricsEnd)
	if err := f.s.fill(ctx, span); err != nil {
		return err
	}
	b := f.s.buf[:min(len(f.s.buf), span)]
	i := bytes.Index(b[len(lyricsBegin):], lyricsEnd)
	switch {
	case i >= 0:
		return f.skipSpan(ctx, int64(len(lyricsBegin)+i+len(lyricsEnd)), at, "Lyrics3 tag")
	case len(b) < span:
		return &TruncatedError{Offset: at, What: "Lyrics3 tag"}
	}
	return &CorruptError{Frame: f.frames, Detail: "malformed Lyrics3 tag"}
}

// lyricsField 报告 b 起点是否是 Lyrics3 v2 字段头:3 个大写字母的标识与 5 位十进制长度。
func lyricsField(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	for _, c := range b[:3] {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return isDigits(b[3:8])
}

func isDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// decimal 解析 isDigits 核对过的十进制数字串。
func decimal(b []byte) int64 {
	var v int64
	for _, c := range b {
		v = v*10 + int64(c-'0')
	}
	return v
}
