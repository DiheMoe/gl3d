package gl3d

// MPEG 音频帧头解析。

// mpegVersion 标识 MPEG 音频版本。MPEG-2.5 是 Fraunhofer 非官方扩展,解码
// 规则等同 MPEG-2,采样率再减半。
type mpegVersion int

const (
	mpeg1 mpegVersion = iota
	mpeg2
	mpeg25
)

func (v mpegVersion) String() string {
	switch v {
	case mpeg1:
		return "1"
	case mpeg2:
		return "2"
	default:
		return "2.5"
	}
}

// lsf 报告是否为低采样率版本(MPEG-2/2.5),两者解码规则相同。
func (v mpegVersion) lsf() bool { return v != mpeg1 }

type channelMode int

const (
	modeStereo channelMode = iota
	modeJointStereo
	modeDualChannel
	modeMono
)

type frameHeader struct {
	version         mpegVersion
	hasCRC          bool
	bitrateKbps     int
	sampleRate      int
	srIdx           int // 版本内采样率序号(0–2),scalefactor band 表按它索引
	padding         int
	channelMode     channelMode
	modeExt         int // 仅 joint stereo 有效:bit0 intensity stereo,bit1 MS stereo
	emphasis        int // 0 为无;非零时解码器不做去加重
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

// Layer I/II 的码率表(kbps),只用于由帧长确认范围外格式的流;LSF 的 Layer II 与
// Layer III 共用 bitrateTableLSF。
var (
	bitrateLayer1MPEG1 = [16]int{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0}
	bitrateLayer2MPEG1 = [16]int{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0}
	bitrateLayer1LSF   = [16]int{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0}
)

// layerFrameSize 返回 b 处帧头的帧长(字节),Layer I、II、III 都按各自的规则计算;
// free format 或无效帧头返回 0。
func layerFrameSize(b [4]byte) int {
	if b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return 0
	}
	versionBits := b[1] >> 3 & 3
	layerBits := b[1] >> 1 & 3
	bitrateIdx := b[2] >> 4
	srIdx := b[2] >> 2 & 3
	if versionBits == 1 || layerBits == 0 || bitrateIdx == 0 || bitrateIdx == 15 || srIdx == 3 {
		return 0
	}
	rate := sampleRateTable[srIdx]
	lsf := versionBits != 3
	if lsf {
		rate >>= 1
		if versionBits == 0 {
			rate >>= 1
		}
	}
	padding := int(b[2] >> 1 & 1)
	switch layerBits {
	case 3: // Layer I:每帧 384 个样本,以 4 字节的 slot 为单位
		kbps := bitrateLayer1MPEG1[bitrateIdx]
		if lsf {
			kbps = bitrateLayer1LSF[bitrateIdx]
		}
		return (12*1000*kbps/rate + padding) * 4
	case 2: // Layer II:每帧 1152 个样本
		kbps := bitrateLayer2MPEG1[bitrateIdx]
		if lsf {
			kbps = bitrateTableLSF[bitrateIdx]
		}
		return 144*1000*kbps/rate + padding
	}
	if lsf {
		return 72*1000*bitrateTableLSF[bitrateIdx]/rate + padding
	}
	return 144*1000*bitrateTableMPEG1[bitrateIdx]/rate + padding
}

// plausibleLayer3 报告 b 起点是否是有效的 Layer III 帧头,与 parseFrameHeader 是否成功
// 一致。它是廉价的预检,不分配内存:parseFrameHeader 每次失败都分配一个错误,逐字节
// 试探垃圾时先用它排除。
func plausibleLayer3(b []byte) bool {
	return len(b) >= 4 && b[1]&0x06 == 0x02 && b[3]&3 != 2 && layerFrameSize([4]byte(b)) != 0
}

func parseFrameHeader(b [4]byte) (frameHeader, error) {
	var h frameHeader
	if b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return h, &CorruptError{Frame: -1, Detail: "lost frame sync"}
	}
	versionBits := b[1] >> 3 & 3
	switch versionBits {
	case 3:
		h.version = mpeg1
	case 2:
		h.version = mpeg2
	case 0:
		h.version = mpeg25
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
		if h.version == mpeg25 {
			h.sampleRate >>= 1
		}
	}

	h.padding = int(b[2] >> 1 & 1)
	h.channelMode = channelMode(b[3] >> 6)
	h.modeExt = int(b[3] >> 4 & 3)
	h.emphasis = int(emphasis)

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
