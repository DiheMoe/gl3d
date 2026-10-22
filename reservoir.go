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
		buf: make([]byte, 0, max+maxMainData),
		max: max,
		win: make([]byte, 0, max+maxMainData),
	}
}

func (r *reservoir) len() int { return len(r.buf) }

// assemble 返回该帧完整 main_data 视图:reservoir 尾部 mainDataBegin 字节起,
// 接上当前帧自带的 main 字节;视图在下一次 assemble 前有效。mainDataBegin
// 超过持有量即 reservoir 越界,属格式损坏。
func (r *reservoir) assemble(mainDataBegin int, frameMain []byte) ([]byte, error) {
	if mainDataBegin > len(r.buf) {
		return nil, &CorruptError{
			Frame:  -1,
			Detail: "main_data_begin exceeds bit reservoir",
		}
	}
	r.win = append(r.win[:0], r.buf[len(r.buf)-mainDataBegin:]...)
	r.win = append(r.win, frameMain...)
	return r.win, nil
}

// advance 在帧处理完成后把该帧自带的 main 字节并入 reservoir,封顶 max。
func (r *reservoir) advance(frameMain []byte) {
	r.buf = append(r.buf, frameMain...)
	if len(r.buf) > r.max {
		copy(r.buf, r.buf[len(r.buf)-r.max:])
		r.buf = r.buf[:r.max]
	}
}
