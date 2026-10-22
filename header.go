package gl3d

// MPEG 音频帧头解析。

// Version 标识 MPEG 音频版本。MPEG-2.5 是 Fraunhofer 非官方扩展,解码
// 规则等同 MPEG-2,采样率再减半。
type Version int

const (
	MPEG1 Version = iota
	MPEG2
	MPEG25
)

func (v Version) String() string {
	switch v {
	case MPEG1:
		return "1"
	case MPEG2:
		return "2"
	default:
		return "2.5"
	}
}

// lsf 报告是否为低采样率版本(MPEG-2/2.5),两者解码规则相同。
func (v Version) lsf() bool { return v != MPEG1 }

type channelMode int

const (
	modeStereo channelMode = iota
	modeJointStereo
	modeDualChannel
	modeMono
)

type frameHeader struct {
	version         Version
	hasCRC          bool
	bitrateKbps     int
	sampleRate      int
	srIdx           int // 版本内采样率序号(0–2),scalefactor band 表按它索引
	padding         int
	channelMode     channelMode
	modeExt         int // 仅 joint stereo 有效:bit0 intensity stereo,bit1 MS stereo
	frameSize       int // 含帧头的整帧字节数
	sideInfoSize    int
	samplesPerFrame int // 每声道采样数:MPEG-1 为 1152,MPEG-2/2.5 为 576
}

func (h *frameHeader) channels() int {
	if h.channelMode == modeMono {
		return 1
	}
	return 2
}

// Layer III 码率表(kbps)。索引 0 为 free format(不支持),15 非法。
var bitrateTableMPEG1 = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
var bitrateTableLSF = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}

var sampleRateTable = [3]int{44100, 48000, 32000}

func parseFrameHeader(b [4]byte) (frameHeader, error) {
	var h frameHeader
	if b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return h, &CorruptError{Frame: -1, Detail: "lost frame sync"}
	}
	versionBits := b[1] >> 3 & 3
	switch versionBits {
	case 3:
		h.version = MPEG1
	case 2:
		h.version = MPEG2
	case 0:
		h.version = MPEG25
	default:
		return h, &CorruptError{Frame: -1, Detail: "reserved MPEG version bits"}
	}
	layerBits := b[1] >> 1 & 3
	bitrateIdx := b[2] >> 4
	srIdxVal := int(b[2] >> 2 & 3)
	emphasis := b[3] & 3
	// 保留的位组合一律判为数据损坏,且须先于"不支持特性"判断,避免把
	// 垃圾字节误报成可识别的范围外格式
	switch {
	case layerBits == 0:
		return h, &CorruptError{Frame: -1, Detail: "reserved layer bits"}
	case bitrateIdx == 15:
		return h, &CorruptError{Frame: -1, Detail: "bad bitrate index 15"}
	case srIdxVal == 3:
		return h, &CorruptError{Frame: -1, Detail: "reserved sample rate index"}
	case emphasis == 2:
		return h, &CorruptError{Frame: -1, Detail: "reserved emphasis"}
	case layerBits == 2:
		return h, &UnsupportedError{Feature: "MPEG Layer II"}
	case layerBits == 3:
		return h, &UnsupportedError{Feature: "MPEG Layer I"}
	case bitrateIdx == 0:
		return h, &UnsupportedError{Feature: "free format bitrate"}
	case emphasis != 0:
		return h, &UnsupportedError{Feature: "non-zero emphasis"}
	}
	h.hasCRC = b[1]&1 == 0

	if h.version.lsf() {
		h.bitrateKbps = bitrateTableLSF[bitrateIdx]
	} else {
		h.bitrateKbps = bitrateTableMPEG1[bitrateIdx]
	}

	h.srIdx = srIdxVal
	h.sampleRate = sampleRateTable[srIdxVal]
	if h.version.lsf() {
		h.sampleRate >>= 1
		if h.version == MPEG25 {
			h.sampleRate >>= 1
		}
	}

	h.padding = int(b[2] >> 1 & 1)
	h.channelMode = channelMode(b[3] >> 6)
	h.modeExt = int(b[3] >> 4 & 3)

	if h.version.lsf() {
		h.frameSize = 72*1000*h.bitrateKbps/h.sampleRate + h.padding
		h.samplesPerFrame = 576
		h.sideInfoSize = 17
		if h.channels() == 1 {
			h.sideInfoSize = 9
		}
	} else {
		h.frameSize = 144*1000*h.bitrateKbps/h.sampleRate + h.padding
		h.samplesPerFrame = 1152
		h.sideInfoSize = 32
		if h.channels() == 1 {
			h.sideInfoSize = 17
		}
	}
	return h, nil
}

// crc16 计算 MPEG 音频 CRC:CRC-16,多项式 0x8005,初值 0xFFFF,不反射。
// Layer III 的覆盖范围是帧头第 16–31 位加整个 side information。
func crc16(data []byte) uint16 { return crc16Update(0xFFFF, data) }

// crc16Update 在已有校验值上继续累积 data;CRC 覆盖范围不连续,须分段计算。
func crc16Update(crc uint16, data []byte) uint16 {
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
