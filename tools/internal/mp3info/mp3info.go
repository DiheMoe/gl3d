// Package mp3info 解析 MP3 文件的 ID3 标签、首个帧头与 Xing/Info/VBRI/LAME 元数据。
// 它独立于 gl3d 解码器,语料工具因此能用被测实现之外的解析器核对清单条目。
package mp3info

import (
	"encoding/binary"
	"fmt"
	"strings"
)

type ID3v2Info struct {
	Version string `json:"version"`
	Size    int    `json:"size"`
}

type XingInfo struct {
	Kind    string  `json:"kind"` // "xing" 或 "info"
	Frames  *uint32 `json:"frames,omitempty"`
	Bytes   *uint32 `json:"bytes,omitempty"`
	TOC     bool    `json:"toc"`
	Quality *uint32 `json:"quality,omitempty"`
}

type VBRIInfo struct {
	Frames uint32 `json:"frames"`
	Bytes  uint32 `json:"bytes"`
}

type LAMEInfo struct {
	Encoder    string `json:"encoder"`
	VBRMethod  int    `json:"vbrMethod"`
	EncDelay   int    `json:"encDelay"`
	EncPadding int    `json:"encPadding"`
}

// Header 是一个 Layer III 帧头描述的格式。
type Header struct {
	MPEGVersion string `json:"mpegVersion"`
	SampleRate  int    `json:"sampleRate"`
	Channels    int    `json:"channels"`
	ChannelMode string `json:"channelMode"`
	BitrateKbps int    `json:"bitrateKbps"`
	FrameSize   int    `json:"frameSize"`
}

type Info struct {
	MPEGVersion string     `json:"mpegVersion"`
	SampleRate  int        `json:"sampleRate"`
	Channels    int        `json:"channels"`
	ChannelMode string     `json:"channelMode"`
	BitrateKbps int        `json:"bitrateKbps"`
	FrameSize   int        `json:"frameSize"`
	ID3v2       *ID3v2Info `json:"id3v2,omitempty"`
	ID3v1       bool       `json:"id3v1"`
	Xing        *XingInfo  `json:"xing,omitempty"`
	VBRI        *VBRIInfo  `json:"vbri,omitempty"`
	LAME        *LAMEInfo  `json:"lame,omitempty"`
	// Audio 是第一个音频帧。首帧是 Xing/Info/VBRI 标签帧时取其后一帧,因为标签帧的
	// 帧头可能与音频帧不同(FFmpeg 在 joint stereo 流中把 Info 帧写成 stereo);
	// 标签帧之后没有有效帧时为 nil。
	Audio *Header `json:"audio,omitempty"`
}

// bitrateTable 按是否 MPEG-1(否则为 MPEG-2/2.5)与帧头的 4 位码率序号给出
// Layer III 码率(kbps)。
var bitrateTable = map[bool][]int{
	true:  {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	false: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
}

var sampleRateTable = []int{44100, 48000, 32000}

var channelModes = []string{"stereo", "joint_stereo", "dual_channel", "mono"}

// Parse 解析 data 开头的 ID3v2 标签、首个帧头及其中的 Xing/Info/VBRI/LAME 元数据,
// 并检测末尾的 ID3v1 标签。顶层格式字段取自首个帧头。
func Parse(data []byte) (*Info, error) {
	off := 0
	var id3v2 *ID3v2Info
	if len(data) >= 10 && string(data[:3]) == "ID3" {
		sz := 0
		for _, b := range data[6:10] {
			if b&0x80 != 0 {
				return nil, fmt.Errorf("invalid ID3v2 syncsafe size byte %#x", b)
			}
			sz = sz<<7 | int(b)
		}
		size := 10 + sz
		if data[3] == 4 && data[5]&0x10 != 0 { // ID3v2.4 的 footer 标志
			size += 10
		}
		if len(data) < size {
			return nil, fmt.Errorf("truncated ID3v2 tag: need %d bytes, have %d", size, len(data))
		}
		id3v2 = &ID3v2Info{Version: fmt.Sprintf("2.%d.%d", data[3], data[4]), Size: size}
		off = size
	}

	first, sideInfo, err := parseHeader(data, off)
	if err != nil {
		return nil, err
	}

	info := &Info{
		MPEGVersion: first.MPEGVersion,
		SampleRate:  first.SampleRate,
		Channels:    first.Channels,
		ChannelMode: first.ChannelMode,
		BitrateKbps: first.BitrateKbps,
		FrameSize:   first.FrameSize,
		ID3v2:       id3v2,
		ID3v1:       len(data) >= 128 && string(data[len(data)-128:len(data)-125]) == "TAG",
	}

	if xo := off + 4 + sideInfo; len(data) >= xo+8 {
		if magic := string(data[xo : xo+4]); magic == "Xing" || magic == "Info" {
			x := &XingInfo{Kind: strings.ToLower(magic)}
			flags := binary.BigEndian.Uint32(data[xo+4:])
			p := xo + 8
			take := func(n int) ([]byte, error) {
				if len(data) < p+n {
					return nil, fmt.Errorf("truncated Xing field at offset %d", p)
				}
				b := data[p : p+n]
				p += n
				return b, nil
			}
			var err error
			if flags&1 != 0 {
				var b []byte
				if b, err = take(4); err != nil {
					return nil, err
				}
				v := binary.BigEndian.Uint32(b)
				x.Frames = &v
			}
			if flags&2 != 0 {
				var b []byte
				if b, err = take(4); err != nil {
					return nil, err
				}
				v := binary.BigEndian.Uint32(b)
				x.Bytes = &v
			}
			if flags&4 != 0 {
				if _, err = take(100); err != nil {
					return nil, err
				}
				x.TOC = true
			}
			if flags&8 != 0 {
				var b []byte
				if b, err = take(4); err != nil {
					return nil, err
				}
				v := binary.BigEndian.Uint32(b)
				x.Quality = &v
			}
			info.Xing = x
			info.LAME = parseLAME(data[p:])
		}
	}

	if vo := off + 36; len(data) >= vo+18 && string(data[vo:vo+4]) == "VBRI" {
		info.VBRI = &VBRIInfo{
			Bytes:  binary.BigEndian.Uint32(data[vo+10:]),
			Frames: binary.BigEndian.Uint32(data[vo+14:]),
		}
	}
	info.Audio = &first
	if info.Xing != nil || info.VBRI != nil {
		info.Audio = nil
		if audio, _, err := parseHeader(data, off+first.FrameSize); err == nil {
			info.Audio = &audio
		}
	}
	return info, nil
}

// ParseHeader 解析 data[off:] 处的 Layer III 帧头。
func ParseHeader(data []byte, off int) (Header, error) {
	h, _, err := parseHeader(data, off)
	return h, err
}

// parseHeader 同 ParseHeader,另外返回 side information 的字节数。
func parseHeader(data []byte, off int) (Header, int, error) {
	var hd Header
	if len(data) < off+4 {
		return hd, 0, fmt.Errorf("truncated frame header at offset %d", off)
	}
	h := data[off:]
	if h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return hd, 0, fmt.Errorf("no frame sync at offset %d", off)
	}
	versionBits := h[1] >> 3 & 3
	if versionBits == 1 {
		return hd, 0, fmt.Errorf("reserved MPEG version bits")
	}
	if layerBits := h[1] >> 1 & 3; layerBits != 1 {
		return hd, 0, fmt.Errorf("not a Layer III frame (layer bits %d)", layerBits)
	}
	var lsf bool
	var srShift uint
	switch versionBits {
	case 3:
		hd.MPEGVersion = "1"
	case 2:
		hd.MPEGVersion, lsf, srShift = "2", true, 1
	case 0:
		hd.MPEGVersion, lsf, srShift = "2.5", true, 2
	}
	bitrateIdx := h[2] >> 4
	if bitrateIdx == 0 {
		return hd, 0, fmt.Errorf("free format bitrate unsupported")
	}
	if bitrateIdx == 15 {
		return hd, 0, fmt.Errorf("bad bitrate index 15")
	}
	hd.BitrateKbps = bitrateTable[!lsf][bitrateIdx]
	srIdx := h[2] >> 2 & 3
	if srIdx == 3 {
		return hd, 0, fmt.Errorf("reserved sample rate index")
	}
	hd.SampleRate = sampleRateTable[srIdx] >> srShift
	padding := int(h[2] >> 1 & 1)
	chanMode := h[3] >> 6
	hd.ChannelMode = channelModes[chanMode]
	hd.Channels = 2
	if chanMode == 3 {
		hd.Channels = 1
	}
	hd.FrameSize = 144*1000*hd.BitrateKbps/hd.SampleRate + padding
	if lsf {
		hd.FrameSize = 72*1000*hd.BitrateKbps/hd.SampleRate + padding
	}
	sideInfo := 32
	if lsf {
		sideInfo = 17
		if hd.Channels == 1 {
			sideInfo = 9
		}
	} else if hd.Channels == 1 {
		sideInfo = 17
	}
	return hd, sideInfo, nil
}

// parseLAME 解析 Xing/Info 块之后的 36 字节 LAME 扩展。不足 36 字节或编码器字段
// 不是可打印 ASCII 时视为没有 LAME 标签,返回 nil。
func parseLAME(b []byte) *LAMEInfo {
	if len(b) < 36 {
		return nil
	}
	enc := b[:9]
	for _, c := range enc {
		if c != 0 && (c < 32 || c > 126) {
			return nil
		}
	}
	return &LAMEInfo{
		Encoder:    string(strings.TrimRight(string(enc), "\x00")),
		VBRMethod:  int(b[9] & 0x0F),
		EncDelay:   int(b[21])<<4 | int(b[22])>>4,
		EncPadding: int(b[22]&0x0F)<<8 | int(b[23]),
	}
}
