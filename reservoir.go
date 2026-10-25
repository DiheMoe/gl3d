package gl3d

// bit reservoir:MPEG-1 不超过 511 字节,MPEG-2/2.5 不超过 255 字节,与
// main_data_begin 的位宽(9/8 bit)自洽。
type reservoir struct {
	buf []byte // 保留的历史 main_data 字节
	max int
	win []byte // assemble 的输出视图,下一次 assemble 时被覆盖
}

// maxMainData 是单帧自带 main data 的上界(最大帧 1441 字节减去帧头与 side info)。
const maxMainData = 1441

func newReservoir(max int) *reservoir {
	return &reservoir{
		buf: make([]byte, 0, max),
		max: max,
		win: make([]byte, 0, max+maxMainData),
	}
}

func (r *reservoir) len() int { return len(r.buf) }

// assemble 返回该帧完整 main_data 视图:reservoir 尾部 mainDataBegin 字节起,
// 接上当前帧自带的 main 字节;视图只保证在本帧解码期间有效,要求 frameMain
// 在此期间保持有效且不变。无回指时直接借用 frameMain。码流从中途开始时
// mainDataBegin 可以超过已收到的数据量:视图开头补 missing 个 0 字节占位,
// 使偏移与完整码流一致,占位字节不是码流数据。
func (r *reservoir) assemble(mainDataBegin int, frameMain []byte) (window []byte, missing int) {
	if mainDataBegin == 0 {
		return frameMain, 0
	}
	missing = max(mainDataBegin-len(r.buf), 0)
	r.win = r.win[:missing]
	clear(r.win)
	r.win = append(r.win, r.buf[len(r.buf)-mainDataBegin+missing:]...)
	r.win = append(r.win, frameMain...)
	return r.win, missing
}

// advance 在帧处理完成后把该帧自带的 main 字节并入 reservoir,封顶 max。
func (r *reservoir) advance(frameMain []byte) {
	if len(frameMain) >= r.max {
		r.buf = r.buf[:r.max]
		copy(r.buf, frameMain[len(frameMain)-r.max:])
		return
	}
	keep := min(len(r.buf), r.max-len(frameMain))
	copy(r.buf, r.buf[len(r.buf)-keep:])
	r.buf = append(r.buf[:keep], frameMain...)
}
