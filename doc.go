// Package gl3d 是纯 Go 的 MPEG-1/2/2.5 Layer III 解码器,只依赖标准库,
// 流式解码,内存有界。
//
// NewDecoder 读取定位首个有效音频帧及确认格式所需的数据后返回,
// ReadPCM 交付交错的 float64 样本,流结束时返回 io.EOF。
//
// 默认是宽松模式。宽松模式下,流中可以容忍的异常——音频之后的垃圾、
// 流中间的标签、损坏或截断的帧、帧数与声明不符等——跳过或容错后
// 继续解码,第一处异常与各项计数记入 Decoder.Report。
// 解码正常结束且 Report.Clean 为真时,同一码流在严格模式下也成功,
// 输出逐字节相同。Clean 为假时,正常结束不代表码流完好。
// Strict 选择严格模式,这些异常都以错误终止解码。
//
// 两种模式下 NewDecoder 都可能返回:
//
//   - 源读取错误与取消,原样返回。源持续返回 (0, nil) 时是 io.ErrNoProgress。
//   - ErrNoAudio:输入为空、只有标签,或首帧前的非标签数据不超过 64 KiB
//     便结束而没有帧。宽松模式下,数据在开头的标签或首帧的帧头中间结束时,
//     也返回它。
//   - UnsupportedError:首帧是 Layer I/II 或 free format。
//   - LimitError:首帧前的非标签数据超过 64 KiB 仍没有帧。
//
// 严格模式下 NewDecoder 还可能返回:
//
//   - UnsupportedError:首帧的 emphasis 非零。
//   - LimitError:开头的 ID3v2 标签累计超过 16 MiB,通常是内嵌的大封面。
//   - CorruptError:开头的 ID3v2 头无效,或首帧中的 Xing/Info/VBRI 头无法解析。
//   - TruncatedError:数据在开头的标签或首帧的帧头中间结束。
//
// 严格模式下,定位首帧、读取其帧头与 Xing/Info/VBRI 头时发现的异常由
// NewDecoder 返回,首帧的帧体(包括被截断的)及其后的异常由 ReadPCM 返回。
// 宽松模式下 ReadPCM 只以 io.EOF、源读取错误、io.ErrNoProgress、取消或
// ErrNoAudio 结束。丢弃、跳过或截断了数据,而一个帧也没有解出、
// 没有交付任何采样时,返回 ErrNoAudio。输出缓冲不足一个采样帧时,
// 两种模式的 ReadPCM 都返回 ErrSmallBuffer,解码不受影响。
// 各错误出现的场合见其类型的说明。
package gl3d
