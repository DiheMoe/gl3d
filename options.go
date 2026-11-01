package gl3d

// Option 是 NewDecoder 的选项。
type Option func(*config)

type config struct {
	strict bool // 零值即宽松模式
}

// Strict 选择严格模式:码流的结构异常都以错误终止解码,
// 即宽松模式下 Report().Clean() 为假的各种情形;
// 开头的 ID3v2 与音频之后可识别的标签是正常结构。
// 不传选项时是宽松模式。
func Strict() Option {
	return func(c *config) { c.strict = true }
}
