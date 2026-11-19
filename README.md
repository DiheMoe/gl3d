# gl3d

[English](README.en.md)

纯 Go 的 MPEG Layer III（MP3）流式解码器。支持 MPEG-1、MPEG-2 和 MPEG-2.5，只依赖标准库，MIT 许可。

## 安装

```sh
go get github.com/DiheMoe/gl3d
```

gl3d 需要 Go 1.27 或更高版本。

## 用法

```go
d, err := gl3d.NewDecoder(ctx, r)
if err != nil {
	return err
}
buf := make([]float64, 4096*d.Channels())
for {
	n, err := d.ReadPCM(ctx, buf)
	if n > 0 {
		consume(buf[:n*d.Channels()])
	}
	if err == io.EOF {
		break
	}
	if err != nil {
		return err
	}
}
```

`NewDecoder` 从任意 `io.Reader` 顺序读取，不做 seek。`ReadPCM` 写入满刻度为 1.0 的交错 `float64` 样本，返回写入的采样帧数。样本不做限幅，可能超出 ±1。`SampleRate` 和 `Channels` 报告流本身的采样率与声道数，因为 gl3d 不做重采样，也不做声道混音。

## 输入与解码模式

默认是宽松模式，流中的异常会跳过或容错后继续解码。传入 `gl3d.Strict()` 可切换为严格模式，这些异常会作为错误返回。下表列出两种模式对各类输入的处理方式。

| 输入 | 宽松模式（默认） | 严格模式 |
|---|---|---|
| MPEG-1（32、44.1、48 kHz）、MPEG-2（16、22.05、24 kHz）与 MPEG-2.5（8、11.025、12 kHz）的 Layer III | 正常解码 | 正常解码 |
| 所有 Layer III 码率，支持 CBR、VBR 和 ABR，码率与 padding 可逐帧变化 | 正常解码 | 正常解码 |
| 单声道、立体声、联合立体声（MS 与 intensity stereo）和双单声道（dual channel） | 正常解码 | 正常解码 |
| 长块、短块、起始块、结束块与混合块，支持跨帧 bit reservoir | 正常解码 | 正常解码 |
| 带或不带 CRC 的帧 | 正常解码 | 正常解码 |
| 开头的 ID3v2.2、2.3 和 2.4 标签 | 跳过 | 跳过 |
| 音频之后的 ID3v1、TAG+、APEv1/v2、Lyrics3 v1/v2 和 ID3v2 标签 | 跳过并记入 `Report.TrailingTags` | 同宽松模式 |
| 首帧中的 Xing/Info 或 VBRI 头 | 读取帧数，该帧不作为音频解码 | 同宽松模式 |
| LAME 标签（含 FFmpeg 写入的标签） | 从开头裁去编码器延迟。帧数已知时，从末尾裁去尾部填充 | 同宽松模式 |
| CRC 不匹配的 LAME 标签 | 忽略帧数与 LAME 字段，输出保留编码器延迟与尾部填充。FFmpeg 和 mpg123 仍会裁剪 | 同宽松模式 |
| VBRI 的 delay 字段和 iTunSMPB | 忽略 | 忽略 |
| 首帧前不超过 64 KiB 的非标签数据 | 跳过 | 跳过 |
| 从中途开始的流（例如未重编码直接切分的片段） | main data 起点早于流开头的 granule 解码为静音，保持时长不变 | 同宽松模式 |
| 空输入、只有标签，或首帧前非标签数据不超过 64 KiB 且没有帧 | `ErrNoAudio` | `ErrNoAudio` |
| 首帧前非标签数据超过 64 KiB 仍没有帧 | `LimitError` | `LimitError` |
| Layer I、Layer II 或 free format | 作为首帧时返回 `UnsupportedError`，后续出现时跳过 | `UnsupportedError` |
| 非零 emphasis（包括保留值） | 照常解码，不做去加重，与 FFmpeg 相同。首帧的值记入 `Report.Emphasis` | `UnsupportedError` |
| 流中途改变 MPEG 版本、采样率或声道数 | 跳过格式与首帧不同的帧 | `UnsupportedError` |
| 帧之间或音频之后的垃圾数据，包括损坏的帧头和 ICY 元数据块 | 跳过，在下一帧或标签处恢复解码 | `CorruptError` |
| MP4、MPEG-TS、WAV 等容器格式 | 不解析。容器字节计入首帧前的数据或垃圾数据 | 同宽松模式 |
| 帧之间的标签，包括拼接文件的 Xing/Info/VBRI 帧 | 跳过 | `CorruptError` |
| CRC 不匹配的帧 | 照常解码 | `CorruptError` |
| 非法 side information，或 `part2_3_length` 之和超出 main data 可用位数 | 丢弃该帧 | `CorruptError` |
| 损坏的 scalefactor 或 Huffman 数据 | 将该声道当前 granule 从损坏处起的谱线置零 | `CorruptError` |
| 缺字节的帧（下一帧在当前帧声明的结束位置之前开始） | 丢弃该帧，在下一帧处恢复解码 | `CorruptError` |
| 数据在帧、帧头或标签中间结束 | 丢弃不完整的部分 | `TruncatedError` |
| 实际解码帧数与 Xing/VBRI 声明的帧数不符 | 照常解码 | 在流结尾返回 `TruncatedError` |
| LAME 尾部填充长于裁去开头延迟后剩余的音频 | 不做尾部裁剪 | 不做尾部裁剪，在流结尾返回 `CorruptError` |
| 首帧中无法解析的 Xing/Info/VBRI 头 | 跳过该帧并忽略其字段 | `CorruptError` |
| 主版本号或修订号为 `0xFF`，或长度字节不是 syncsafe 的 ID3v2 头 | 当作垃圾数据处理 | `CorruptError` |
| 未知主版本或带未定义标志位的 ID3v2 头 | 按其声明的长度跳过 | `CorruptError` |
| 格式错误的 APE 或 Lyrics3 标签 | 当作垃圾数据跳过 | `CorruptError` |
| 累计超过 16 MiB 的标签 | 跳过 | `LimitError` |

凡是仅严格模式返回错误的情形，宽松模式都会把问题记入 `Decoder.Report`，此时 `Report.Clean` 返回 false。宽松模式正常结束且 `Report.Clean` 返回 true 时，以严格模式解码同一码流也会成功，且输出完全一致。如果宽松模式丢弃、跳过或截断了数据，最终一个帧也没有解出，则返回 `ErrNoAudio`。

严格模式下，打开流时发现的问题由 `NewDecoder` 返回，其余由 `ReadPCM` 返回。打开流会读取开头的标签、首帧帧头以及 Xing/Info/VBRI 头。gl3d 只跳过元数据标签，不解析其内容。完整的错误契约见包文档。

## 验证状态

- intensity stereo（MPEG-1 和 LSF）目前只由单元测试与构造码流覆盖。测试语料库和 ISO 11172-4 一致性码流中都不包含它。
- LSF intensity 的位置与增益遵循 ISO 13818-3。标准未明确规定的两种情形遵循 FFmpeg 和 mpg123。一种是最高 scalefactor band 下面一带未用 intensity 编码时，最高带的位置如何取值。另一种是 MPEG-1 中每个短窗各自确定 intensity 界限。
- 16、22.05 和 24 kHz 的 scalefactor band 宽度，以及 LSF scalefactor 长度与分区，均与 ISO 13818-3 一致。

## 许可证

MIT。见 [LICENSE.txt](LICENSE.txt)。
