package gl3d

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// ID3v2、Xing/Info、LAME 扩展与 VBRI 标签解析。所有多字节字段大端。

// parseID3v2Header 解析 10 字节 ID3v2 头。size 为标签总字节数(含 10
// 字节头与体,不含 footer);size 字段是 syncsafe,每字节仅低 7 位有效。
// footer 仅 ID3v2.4 可出现,存在时调用方须在 size 之外再跳 10 字节。
func parseID3v2Header(b [10]byte) (size int, footer bool, err error) {
	if b[0] != 'I' || b[1] != 'D' || b[2] != '3' {
		return 0, false, fmt.Errorf("not an ID3v2 header")
	}
	sz := 0
	for _, c := range b[6:10] {
		if c&0x80 != 0 {
			return 0, false, fmt.Errorf("invalid syncsafe size byte %#x", c)
		}
		sz = sz<<7 | int(c)
	}
	return 10 + sz, b[3] == 4 && b[5]&0x10 != 0, nil
}

type xingInfo struct {
	kind    string // "Xing"=VBR/ABR,"Info"=CBR;统一小写存储
	frames  *uint32
	bytes   *uint32
	toc     bool
	quality *uint32
}

// parseXing 从 magic 开始解析 Xing/Info 头;flags 低 4 位依次标记
// frames/bytes/TOC(100 字节)/quality 是否存在,存在的字段按此顺序紧随。
// 返回的 lameOff 是其后 LAME 扩展的起始偏移。
func parseXing(b []byte) (x *xingInfo, lameOff int, err error) {
	if len(b) < 8 {
		return nil, 0, fmt.Errorf("truncated Xing header")
	}
	x = &xingInfo{kind: strings.ToLower(string(b[:4]))}
	flags := binary.BigEndian.Uint32(b[4:8])
	p := 8
	take := func(n int) ([]byte, error) {
		if len(b) < p+n {
			return nil, fmt.Errorf("truncated Xing field at offset %d", p)
		}
		s := b[p : p+n]
		p += n
		return s, nil
	}
	if flags&1 != 0 {
		f, err := take(4)
		if err != nil {
			return nil, 0, err
		}
		v := binary.BigEndian.Uint32(f)
		x.frames = &v
	}
	if flags&2 != 0 {
		f, err := take(4)
		if err != nil {
			return nil, 0, err
		}
		v := binary.BigEndian.Uint32(f)
		x.bytes = &v
	}
	if flags&4 != 0 {
		if _, err := take(100); err != nil {
			return nil, 0, err
		}
		x.toc = true
	}
	if flags&8 != 0 {
		f, err := take(4)
		if err != nil {
			return nil, 0, err
		}
		v := binary.BigEndian.Uint32(f)
		x.quality = &v
	}
	return x, p, nil
}

type lameTag struct {
	encoder    string
	vbrMethod  int
	encDelay   int
	encPadding int
}

// parseLAMETag 读取 Xing/Info 字段区之后的 36 字节 LAME 扩展标签;不足
// 36 字节或编码器字段含不可打印字节时返回 ok=false,视为无 LAME 标签。
func parseLAMETag(b []byte) (l *lameTag, ok bool) {
	if len(b) < 36 {
		return nil, false
	}
	for _, c := range b[:9] {
		if c != 0 && (c < 32 || c > 126) {
			return nil, false
		}
	}
	// enc_delay 与 enc_padding 各 12 bit,按 [d8][d4p4][p8] 紧凑存放
	return &lameTag{
		encoder:    strings.TrimRight(string(b[:9]), "\x00"),
		vbrMethod:  int(b[9] & 0x0F),
		encDelay:   int(b[21])<<4 | int(b[22])>>4,
		encPadding: int(b[22]&0x0F)<<8 | int(b[23]),
	}, true
}

// infoTagCRCValid 校验 LAME 扩展末尾、位于帧内 crcPos 的 Info 标签 CRC
// (CRC-16/ARC:反射多项式 0xA001、初值 0)。写入方的覆盖范围不一:LAME
// 覆盖 CRC 字段之前的全部字节;FFmpeg 固定覆盖帧首 190 字节,计算时 CRC 字段
// 置 0,帧不足 190 字节时帧尾之后按 0 计。两种约定只在 CRC 字段恰位于第 190
// 字节时一致,任一吻合即视为有效。
func infoTagCRCValid(frame []byte, crcPos int) bool {
	stored := binary.BigEndian.Uint16(frame[crcPos:])
	c := crc16ARC(0, frame[:crcPos])
	if c == stored {
		return true
	}
	if crcPos+2 > 190 {
		return false
	}
	var zeros [190]byte
	end := min(len(frame), 190)
	c = crc16ARC(c, zeros[:2])
	c = crc16ARC(c, frame[crcPos+2:end])
	return crc16ARC(c, zeros[:190-end]) == stored
}

// crc16ARC 在已有校验值 crc 上继续累积 data 的 CRC-16/ARC。
func crc16ARC(crc uint16, data []byte) uint16 {
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

type vbriInfo struct {
	frames uint32
	bytes  uint32
}

// parseVBRI 从 magic 开始解析 VBRI 头;固定部分 26 字节,只取 bytes/frames
// 供时长与帧数核对,TOC 与 delay 字段不使用。
func parseVBRI(b []byte) (*vbriInfo, error) {
	if len(b) < 26 {
		return nil, fmt.Errorf("truncated VBRI header")
	}
	return &vbriInfo{
		bytes:  binary.BigEndian.Uint32(b[10:]),
		frames: binary.BigEndian.Uint32(b[14:]),
	}, nil
}
