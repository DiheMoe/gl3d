package gl3d

import (
	"errors"
	"fmt"
)

// ErrNoAudio 表示没有可以解码的音频。两种模式都在 NewDecoder 返回它:
// 输入为空、只有标签,或首帧前的非标签数据不超过 64 KiB 便结束而没有帧,
// 超过 64 KiB 则是 LimitError。宽松模式下,
// 开头的标签或首帧的帧头在数据结束处不完整时,NewDecoder 也返回它,
// 严格模式报 TruncatedError。丢弃、跳过或截断了数据,
// 而一个帧也没有解出、没有交付任何采样时,宽松模式的 ReadPCM 返回它。
var ErrNoAudio = errors.New("gl3d: no audio data")

// UnsupportedError 表示已识别但不支持的格式或特性:Layer I/II、free format、
// 非零 emphasis,或流中途改变 MPEG 版本、采样率、声道数。
// 首帧是 Layer I/II 或 free format 时,两种模式都在 NewDecoder 返回。
// 其余情形只在严格模式出现,其中首帧的非零 emphasis 由 NewDecoder 返回,
// 其余由 ReadPCM 返回。宽松模式对非零 emphasis 照常解码(不做去加重),
// 跳过流中途出现的 Layer I/II、free format 与格式不同的帧,
// 都记入 Report(Issue.Kind 为 IssueUnsupported)。
type UnsupportedError struct {
	Feature string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("gl3d: unsupported: %s", e.Feature)
}

// TruncatedError 表示帧或标签内部不完整,或实际计数与可信的长度声明不一致,
// 不得当作 EOF 成功处理。Offset 为出错位置在流中的字节偏移。
// 只在严格模式出现。数据在开头的标签或首帧的帧头中间结束时由 NewDecoder 返回,
// 其余由 ReadPCM 返回,首帧的帧体被截断也是。
// 宽松模式下,数据在首帧之前结束时 NewDecoder 返回 ErrNoAudio,没有 Report 可查。
// 宽松模式的其余情形丢弃不完整的部分,记入 Report(Issue.Kind 为 IssueTruncated)。
// 其中帧数与声明不符时不丢弃任何数据,只记录。
type TruncatedError struct {
	Offset int64
	What   string
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("gl3d: truncated %s at byte %d", e.What, e.Offset)
}

// CorruptError 表示码流格式损坏:保留值、非法长度、CRC 不匹配、
// 非法 Huffman 数据、流中间的标签等。
// Frame 为出错音频帧的序号(不含标签帧),无法定位到帧时为 -1。
// 只在严格模式出现。开头的 ID3v2 头无效,
// 或首帧中的 Xing/Info/VBRI 头无法解析时,由 NewDecoder 返回,
// 其余由 ReadPCM 返回。宽松模式跳过或容错损坏的部分,
// 把同样的异常记入 Report(Issue.Kind 为 IssueCorrupt)。
type CorruptError struct {
	Frame  int64
	Detail string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("gl3d: corrupt stream at frame %d: %s", e.Frame, e.Detail)
}

// LimitError 表示超过资源预算:单个流的标签跳过累计 16 MiB,
// 或首帧前非标签数据的同步搜索 64 KiB。
// 首帧前的非标签数据超过 64 KiB 仍没有帧时,两种模式都在 NewDecoder 返回。
// 标签累计超过 16 MiB 只在严格模式出现,开头的标签由 NewDecoder 返回,
// 其后的由 ReadPCM 返回。宽松模式记入 Report(Issue.Kind 为 IssueLimit)
// 并继续跳过。
type LimitError struct {
	Budget string
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("gl3d: resource limit exceeded: %s", e.Budget)
}
