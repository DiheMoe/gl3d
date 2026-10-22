package gl3d

import (
	"errors"
	"fmt"
)

// ErrNoAudio 表示输入为空或只有标签数据、不含任何音频帧。
var ErrNoAudio = errors.New("gl3d: no audio data")

// UnsupportedError 表示已识别但不支持的格式或特性:Layer I/II、free
// format、非零 emphasis,或流中途改变 MPEG 版本、采样率、声道数。
type UnsupportedError struct {
	Feature string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("gl3d: unsupported: %s", e.Feature)
}

// TruncatedError 表示帧或标签内部不完整,或实际计数与可信的长度声明不
// 一致;不得当作 EOF 成功处理。Offset 为出错位置在流中的字节偏移。
type TruncatedError struct {
	Offset int64
	What   string
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("gl3d: truncated %s at byte %d", e.What, e.Offset)
}

// CorruptError 表示码流格式损坏:保留值、非法长度、CRC 不匹配、非法
// Huffman 数据、reservoir 越界等。Frame 为出错音频帧的序号(不含标签帧),
// 无法定位到帧时为 -1。
type CorruptError struct {
	Frame  int64
	Detail string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("gl3d: corrupt stream at frame %d: %s", e.Frame, e.Detail)
}

// LimitError 表示超过资源预算:单个流的标签跳过累计 16 MiB,或首帧前
// 非标签数据的同步搜索 64 KiB。
type LimitError struct {
	Budget string
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("gl3d: resource limit exceeded: %s", e.Budget)
}
