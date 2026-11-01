package gl3d

import (
	"errors"
	"log/slog"
	"strings"
)

// Report 汇总解码器在流中遇到的结构。随时可查;
// ReadPCM 返回 io.EOF 或 ErrSmallBuffer 以外的错误之后为最终值。
type Report struct {
	DeclaredFrames    int64  // Xing/VBRI 声明的音频帧数(MPEG 帧,不是采样帧);-1 表示没有采用的声明,声明为 0 视为未知
	AudioFrames       int64  // 已解码的音频帧数(MPEG 帧,不是采样帧),不含标签帧
	DroppedFrames     int64  // 宽松模式丢弃的帧:帧内另有一帧开始(帧长与数据对不上),或 side info 非法
	ConcealedGranules int64  // 宽松模式容错的 granule(每声道各计):scalefactor 或谱数据出错,余下的谱线置零
	CRCErrors         int64  // 宽松模式 CRC 不符的帧;side info 仍合法时照常解码,否则另计入 DroppedFrames
	SkippedBytes      int64  // 宽松模式跳过的字节:垃圾、分帧层丢弃的帧、格式不同的帧、损坏的标签、未知尾部数据;首帧之前的字节不计
	Resyncs           int64  // 宽松模式跳过数据之后重新找到帧或标签的次数
	MidstreamTags     int64  // 流中间跳过的标签与 Xing/Info/VBRI 标签帧的个数
	Truncated         bool   // 流在帧、帧头或标签中间结束,宽松模式丢弃了不完整的部分
	Emphasis          int    // 首帧的 emphasis 字段;非零时宽松模式照常解码,不做去加重
	TrailingTags      TagSet // 流尾识别出的标签
	First             *Issue // 第一处异常;nil 表示没有
}

// Clean 报告到目前为止是否没有遇到异常。以源读取错误或取消结束时,它只说明
// 已经读到的部分。
func (r Report) Clean() bool { return r.First == nil }

// note 把 err 记为第一处异常,frame 与 offset 是发现它的位置;错误自带的位置
// (TruncatedError 的偏移、CorruptError 的帧号)优先。已有记录时不覆盖;只记录
// 流的格式问题与资源限制,源读取错误与取消不属于流的结构。
func (r *Report) note(err error, frame, offset int64) {
	if r.First != nil {
		return
	}
	kind := issueKind(err)
	if kind == 0 {
		return
	}
	var (
		t *TruncatedError
		c *CorruptError
	)
	if errors.As(err, &t) {
		offset = t.Offset
	}
	if errors.As(err, &c) {
		frame = c.Frame
	}
	r.First = &Issue{Kind: kind, Frame: frame, Offset: offset}
}

// issueKind 返回 err 对应的异常类别;不是流的格式问题或资源限制时返回 0。分帧层产生的
// 错误不经包装,先按动态类型直接判断,不分配内存;包装过的错误才走 errors.As。
func issueKind(err error) IssueKind {
	switch err.(type) {
	case *TruncatedError:
		return IssueTruncated
	case *CorruptError:
		return IssueCorrupt
	case *UnsupportedError:
		return IssueUnsupported
	case *LimitError:
		return IssueLimit
	}
	return wrappedIssueKind(err)
}

// wrappedIssueKind 沿 err 的包装链找异常类别。errors.As 的目标变量会逃逸到堆上,单独成
// 函数,只有包装过或不属于任何类别的错误才付这个代价。
func wrappedIssueKind(err error) IssueKind {
	var (
		t *TruncatedError
		c *CorruptError
		u *UnsupportedError
		l *LimitError
	)
	switch {
	case errors.As(err, &t):
		return IssueTruncated
	case errors.As(err, &c):
		return IssueCorrupt
	case errors.As(err, &u):
		return IssueUnsupported
	case errors.As(err, &l):
		return IssueLimit
	}
	return 0
}

// LogValue 让 slog 把报告展开为字段。
func (r Report) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.Bool("clean", r.Clean()),
		slog.Int64("audio_frames", r.AudioFrames),
		slog.Int64("declared_frames", r.DeclaredFrames),
	}
	if r.DroppedFrames != 0 {
		attrs = append(attrs, slog.Int64("dropped_frames", r.DroppedFrames))
	}
	if r.ConcealedGranules != 0 {
		attrs = append(attrs, slog.Int64("concealed_granules", r.ConcealedGranules))
	}
	if r.CRCErrors != 0 {
		attrs = append(attrs, slog.Int64("crc_errors", r.CRCErrors))
	}
	if r.SkippedBytes != 0 {
		attrs = append(attrs, slog.Int64("skipped_bytes", r.SkippedBytes))
	}
	if r.Resyncs != 0 {
		attrs = append(attrs, slog.Int64("resyncs", r.Resyncs))
	}
	if r.Truncated {
		attrs = append(attrs, slog.Bool("truncated", true))
	}
	if r.MidstreamTags != 0 {
		attrs = append(attrs, slog.Int64("midstream_tags", r.MidstreamTags))
	}
	if r.Emphasis != 0 {
		attrs = append(attrs, slog.Int("emphasis", r.Emphasis))
	}
	if r.TrailingTags != 0 {
		attrs = append(attrs, slog.String("trailing_tags", r.TrailingTags.String()))
	}
	if r.First != nil {
		attrs = append(attrs, slog.Group("first",
			slog.String("kind", r.First.Kind.String()),
			slog.Int64("frame", r.First.Frame),
			slog.Int64("offset", r.First.Offset)))
	}
	return slog.GroupValue(attrs...)
}

// Issue 是一处异常:类别,以及它在流中的位置——此前已解码的音频帧数(MPEG 帧,
// 不含标签帧;无法定位到帧时为 -1)与异常所在的帧或标签的起点;流结束时才发现的
// 异常(帧数与声明不符、gapless 填充超出输出)偏移为流的结尾。
type Issue struct {
	Kind   IssueKind
	Frame  int64
	Offset int64
}

// IssueKind 是异常的类别。
type IssueKind int

const (
	IssueTruncated   IssueKind = iota + 1 // 帧或标签不完整,或帧数与声明不符
	IssueCorrupt                          // 格式损坏
	IssueUnsupported                      // 范围外的格式或特性
	IssueLimit                            // 超过资源预算
)

func (k IssueKind) String() string {
	switch k {
	case IssueTruncated:
		return "truncated"
	case IssueCorrupt:
		return "corrupt"
	case IssueUnsupported:
		return "unsupported"
	case IssueLimit:
		return "limit"
	}
	return "unknown"
}

// TagSet 是标签种类的集合。
type TagSet uint8

const (
	TagID3v1    TagSet = 1 << iota // 128 字节的 ID3v1 标签
	TagID3v1Ext                    // ID3v1 之前 227 字节的扩展标签(TAG+)
	TagAPEv2                       // APEv2 标签,也包括 APEv1
	TagLyrics3                     // Lyrics3 v1 或 v2 标签
	TagID3v2                       // 音频之后的 ID3v2 标签
)

var tagNames = [...]string{"ID3v1", "TAG+", "APEv2", "Lyrics3", "ID3v2"}

func (s TagSet) String() string {
	var names []string
	for i, name := range tagNames {
		if s&(1<<i) != 0 {
			names = append(names, name)
		}
	}
	return strings.Join(names, "|")
}
