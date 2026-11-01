package gl3d

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var writeVariants = flag.String("write-variants", "", "write the variants of corpus/variants.json (except in-test-only ones) to this directory")

// variant 是冻结语料的一个变体:base 文件经 ops 改写。期望与两个参考的输出长度
// 在编写清单时用冻结版本的 FFmpeg、mpg123 按默认的 gapless 处理测得(命令见清单的 lengthCommands)。
type variant struct {
	Name       string        `json:"name"`
	Base       string        `json:"base"`
	Class      int           `json:"class"` // 1:音频完好、外围结构异常;2:音频损坏或拼接
	InTestOnly bool          `json:"inTestOnly,omitempty"`
	Ops        []variantOp   `json:"ops"`
	SHA256     string        `json:"sha256"` // variantBytes 的十六进制 SHA-256:参考长度针对这些字节测得,字节变了就要重测
	Expect     variantExpect `json:"expect"`
	Note       string        `json:"note,omitempty"` // 两个参考不一致或期望不显然时的裁决理由
}

// variantOp 是一个改写操作。Frame 是 base 的音频帧序号(不含标签帧,从 0 起;
// 等于帧数时指最后一帧之后),Off 是帧内字节偏移。
type variantOp struct {
	Op    string `json:"op"`
	Frame int    `json:"frame,omitempty"`
	Off   int    `json:"off,omitempty"`
	N     int    `json:"n,omitempty"`
	Seed  uint64 `json:"seed,omitempty"`
	File  string `json:"file,omitempty"`
}

type variantExpect struct {
	FFmpeg int64  `json:"ffmpeg"`           // FFmpeg 输出的采样帧数;超大标签变体为 -1(不测)
	MPG123 int64  `json:"mpg123"`           // mpg123 输出的采样帧数;同上
	Strict string `json:"strict"`           // 严格模式的结果类别,取 errClass 的值:ok、corrupt、truncated、unsupported、limit
	Frames int64  `json:"frames,omitempty"` // 第一类:宽松模式交付的采样帧数
	// 第一类:交付的 PCM 与完好 base 的输出(下称 intact)的关系。
	// intact:逐样本相同。
	// prefix:在较短者的长度内与 intact 逐样本相同(尾部被截去)。
	// extra:ops 是单个 copy-frames。前 len(intact) 个样本与 intact 相同(尾部填充在声明位置丢弃),
	// 其后恰是复制进来的各帧,与 base 中原帧的解码逐样本相同(前几帧除外:它们的 main data 回指流的最后一帧)。
	// offset:ops 是单个从第 0 帧起的 drop-frames。交付的第 j 个采样帧等于 intact 的第 j+N·spf 个,
	// 重叠范围内逐样本相同(第一帧除外:它回指的 main data 随被丢弃的帧一起没了)。
	PCM string `json:"pcm,omitempty"`
	// 第二类:第一处损坏所在的音频帧;尾部从 TailBase(默认 base)的第 TailFrom 帧起与之一致,
	// TailFrom 为 nil 表示损坏之后没有可比较的尾部
	FirstDamage int    `json:"firstDamage,omitempty"`
	TailBase    string `json:"tailBase,omitempty"`
	TailFrom    *int   `json:"tailFrom,omitempty"`
}

// variantManifest 是 corpus/variants.json 的全部字段。声明齐全并禁止未知字段,
// 拼错的键(例如 tailFrm)才会报错,而不是被当作"没有尾部"。
type variantManifest struct {
	Version        int `json:"version"`
	ReferenceTools struct {
		FFmpeg         string `json:"ffmpeg"`
		MPG123         string `json:"mpg123"`
		LengthCommands struct {
			FFmpeg string `json:"ffmpeg"`
			MPG123 string `json:"mpg123"`
		} `json:"lengthCommands"`
		LengthUnits string `json:"lengthUnits"`
	} `json:"referenceTools"`
	Variants []variant `json:"variants"`
}

func loadVariants(t *testing.T) []variant {
	t.Helper()
	data, err := os.ReadFile("corpus/variants.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m variantManifest
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m.Variants
}

// variantBase 读入 base 文件并返回它的音频帧起点与音频结尾。
func variantBase(t *testing.T, path string) ([]byte, []int, int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	starts, end := audioFrames(t, data)
	return data, starts, end
}

// randomBytes 是由 seed 决定的伪随机字节,变体文件因此可以复现。
func randomBytes(n int, seed uint64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(splitmix64(seed<<32 | uint64(i)))
	}
	return b
}

// isAppendOp 报告操作是否只在当前结尾追加。第一个操作之后只允许追加:其余操作
// (定位操作、pad-after-id3v2、huge-id3v2、emphasis-all)的位置取自 base 的帧表,
// 前面有改动就不再对得上。
func isAppendOp(op string) bool { return strings.HasPrefix(op, "append-") }

// positionalOps 是按 base 帧表定位的操作。
var positionalOps = map[string]bool{
	"insert-id3v2": true, "insert-id3v1": true, "insert-icy": true, "insert-random": true,
	"delete": true, "truncate": true, "overwrite-random": true, "break-sync": true,
	"flip-protection": true, "crc-field": true, "big-values": true,
	"drop-frames": true, "copy-frames": true,
}

// frameOps 按帧序号作用于整帧或帧头、side info 的固定字段,没有帧内偏移,因此不允许 Off。
var frameOps = map[string]bool{
	"break-sync": true, "flip-protection": true, "crc-field": true, "big-values": true,
	"drop-frames": true, "copy-frames": true,
}

// id3v2Len 返回开头 ID3v2 标签(含 footer)的字节数,没有则为 0。
func id3v2Len(t *testing.T, data []byte) int {
	t.Helper()
	if len(data) < 10 || string(data[:3]) != "ID3" {
		return 0
	}
	size, footer, err := parseID3v2Header([10]byte(data[:10]))
	if err != nil {
		t.Fatal(err)
	}
	if footer {
		size += 10
	}
	return size
}

// usesN 是读取 N 的操作;N 为 0 时它们什么也不做。
var usesN = map[string]bool{
	"append-zeros": true, "append-random": true, "insert-id3v2": true, "insert-random": true,
	"pad-after-id3v2": true, "huge-id3v2": true, "delete": true, "overwrite-random": true,
	"drop-frames": true, "copy-frames": true,
}

// inFrameOps 是作用在某个音频帧内部的操作:Frame 必须是真实的帧(不能是"最后一帧之后"),
// 帧内偏移与改动的字节不能越出这一帧,否则改动落进相邻的帧,变体就不是清单描述的样子。
var inFrameOps = map[string]bool{
	"break-sync": true, "flip-protection": true, "crc-field": true, "big-values": true,
	"delete": true, "overwrite-random": true, "truncate": true,
	"insert-id3v2": true, "insert-id3v1": true, "insert-icy": true, "insert-random": true,
}

// frameShape 返回第一个音频帧的每声道采样数、声道数,以及它是否 MPEG-2/2.5(LSF)。
func frameShape(t *testing.T, data []byte, starts []int) (spf, ch int, lsf bool) {
	t.Helper()
	h, err := parseFrameHeader([4]byte(data[starts[0] : starts[0]+4]))
	if err != nil {
		t.Fatal(err)
	}
	return h.samplesPerFrame, h.channels(), h.version.lsf()
}

// skipHugeInShort 在 -short 下跳过 17 MiB 的超大标签变体,短测试与竞态检测因此不生成也不解码它。
func skipHugeInShort(t *testing.T, v variant) {
	t.Helper()
	if v.InTestOnly && testing.Short() {
		t.Skip("large in-test-only variant skipped in -short mode")
	}
}

// variantBytes 由 base 与 ops 生成变体的字节。
func variantBytes(t *testing.T, v variant) []byte {
	t.Helper()
	data, starts, end := variantBase(t, v.Base)
	out := slices.Clone(data)
	for i, op := range v.Ops {
		if i > 0 && !isAppendOp(op.Op) {
			t.Fatalf("%s: %s must be the first op", v.Name, op.Op)
		}
		if frameOps[op.Op] && op.Off != 0 {
			t.Fatalf("%s: %s takes no off", v.Name, op.Op)
		}
		p := 0
		if positionalOps[op.Op] {
			switch {
			case op.Frame >= 0 && op.Frame < len(starts):
				p = starts[op.Frame] + op.Off
			case op.Frame == len(starts):
				p = end + op.Off
			default:
				t.Fatalf("%s: %s frame %d out of range [0, %d]", v.Name, op.Op, op.Frame, len(starts))
			}
			if p > len(out) || (op.Op == "delete" || op.Op == "overwrite-random") && p+op.N > len(out) {
				t.Fatalf("%s: %s position %d beyond %d bytes", v.Name, op.Op, p, len(out))
			}
		}
		switch op.Op {
		case "append-id3v1":
			out = append(out, id3v1Tag()...)
		case "append-apev2":
			out = append(out, apeTag(true, [2]string{"Title", "gl3d"})...)
		case "append-apev2-footer":
			out = append(out, apeTag(false, [2]string{"Title", "gl3d"})...)
		case "append-lyrics3v2":
			out = append(out, lyrics3v2("la la")...)
		case "append-tagplus":
			out = append(out, tagPlus()...)
		case "append-id3v2-footer":
			out = append(out, rawID3v2(4, 0x10, []byte("TIT2\x00\x00\x00\x05\x00\x00\x03gl3d"), true)...)
		case "append-zeros":
			out = append(out, make([]byte, op.N)...)
		case "append-random":
			out = append(out, randomBytes(op.N, op.Seed)...)
		case "append-file", "append-audio":
			extra, err := os.ReadFile(op.File)
			if err != nil {
				t.Fatal(err)
			}
			if op.Op == "append-audio" {
				extra = extra[id3v2Len(t, extra):]
			}
			out = append(out, extra...)
		case "insert-id3v2":
			out = slices.Insert(out, p, id3v2Tag(op.N)...)
		case "insert-id3v1":
			out = slices.Insert(out, p, id3v1Tag()...)
		case "insert-icy":
			// ICY 元数据块:一个长度字节(以 16 字节为单位)加补零的文本
			block := make([]byte, 33)
			block[0] = 2
			copy(block[1:], "StreamTitle='gl3d';")
			out = slices.Insert(out, p, block...)
		case "insert-random":
			out = slices.Insert(out, p, randomBytes(op.N, op.Seed)...)
		case "pad-after-id3v2":
			out = slices.Insert(out, id3v2Len(t, out), make([]byte, op.N)...)
		case "delete":
			out = slices.Delete(out, p, p+op.N)
		case "truncate":
			out = out[:p]
		case "overwrite-random":
			copy(out[p:p+op.N], randomBytes(op.N, op.Seed))
		case "break-sync":
			out[p] = 0
		case "flip-protection":
			out[p+1] ^= 0x01
		case "crc-field":
			h, err := parseFrameHeader([4]byte(out[p : p+4]))
			if err != nil {
				t.Fatal(err)
			}
			if !h.hasCRC {
				t.Fatalf("%s: crc-field: frame %d has no CRC", v.Name, op.Frame)
			}
			out[p+4] ^= 0xFF
		case "emphasis-all":
			for _, s := range starts {
				out[s+3] |= 0x01
			}
		case "big-values":
			h, err := parseFrameHeader([4]byte(out[p : p+4]))
			if err != nil {
				t.Fatal(err)
			}
			mono := h.channels() == 1
			// gr0/ch0 的 big_values 紧跟在 part2_3_length 之后;side info 在帧头与可选 CRC 之后
			var bit int
			if h.version == MPEG1 {
				private, scfsi := 3, 8
				if mono {
					private, scfsi = 5, 4
				}
				bit = 9 + private + scfsi + 12
			} else {
				private := 2
				if mono {
					private = 1
				}
				bit = 8 + private + 12
			}
			side := p + 4
			if h.hasCRC {
				side += 2
			}
			setBits(out[side:], bit, 9, 511)
		case "drop-frames", "copy-frames":
			if op.N <= 0 || op.Frame+op.N > len(starts) {
				t.Fatalf("%s: %s frames [%d, %d) out of range", v.Name, op.Op, op.Frame, op.Frame+op.N)
			}
			to := end
			if op.Frame+op.N < len(starts) {
				to = starts[op.Frame+op.N]
			}
			if op.Op == "drop-frames" {
				out = slices.Delete(out, p, to)
			} else {
				out = slices.Insert(out, end, data[p:to]...)
			}
		case "huge-id3v2":
			out = append(id3v2Tag(op.N), out...)
		default:
			t.Fatalf("%s: unknown op %q", v.Name, op.Op)
		}
	}
	return out
}

// 清单自洽:名字唯一;base、File、tailBase 存在;第一个操作之后只有追加操作;读取 N 的操作
// N > 0,作用在帧内的操作不越出所在的帧;append-tagplus 紧跟 append-id3v1;第一类有
// frames 与 pcm 而没有第二类的字段,两个参考长度一致时 frames 等于它们,不一致时有 note 且
// frames 至少等于其中之一;第二类有 firstDamage 而没有 frames、pcm,firstDamage、tailFrom
// 落在对应文件的帧数范围内,尾部来源与 base 的帧长、声道数相同;严格模式结果类别合法;
// 每个变体都能生成,字节与清单记录的 SHA-256 相同。
func TestVariantsManifest(t *testing.T) {
	vs := loadVariants(t)
	if len(vs) == 0 {
		t.Fatal("no variants")
	}
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	seen := map[string]bool{}
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			if v.Name == "" || seen[v.Name] {
				t.Fatalf("name %q empty or duplicate", v.Name)
			}
			seen[v.Name] = true
			if !exists(v.Base) {
				t.Fatalf("base %s missing", v.Base)
			}
			base, baseStarts, _ := variantBase(t, v.Base)
			if len(v.Ops) == 0 {
				t.Fatal("no ops")
			}
			for i, op := range v.Ops {
				if i > 0 && !isAppendOp(op.Op) {
					t.Errorf("op %s at index %d: only append-* ops may follow the first op", op.Op, i)
				}
				if (op.Op == "append-file" || op.Op == "append-audio") && !exists(op.File) {
					t.Errorf("%s file %q missing", op.Op, op.File)
				}
				if usesN[op.Op] && op.N <= 0 {
					t.Errorf("%s needs n > 0", op.Op)
				}
				if op.Op == "append-tagplus" && (i+1 == len(v.Ops) || v.Ops[i+1].Op != "append-id3v1") {
					t.Errorf("append-tagplus at index %d must be followed by append-id3v1", i)
				}
				if inFrameOps[op.Op] {
					if op.Frame < 0 || op.Frame >= len(baseStarts) {
						t.Errorf("%s frame %d, want 0..%d (the base's audio frames)", op.Op, op.Frame, len(baseStarts)-1)
						continue
					}
					h, err := parseFrameHeader([4]byte(base[baseStarts[op.Frame] : baseStarts[op.Frame]+4]))
					if err != nil {
						t.Fatal(err)
					}
					// 改写与删除触及 N 个字节;其余操作在 Off 处生效,Off 至少落在帧内
					touched := 1
					if op.Op == "delete" || op.Op == "overwrite-random" {
						touched = op.N
					}
					if op.Off < 0 || op.Off+touched > h.frameSize {
						t.Errorf("%s off %d touching %d bytes leaves frame %d (%d bytes)", op.Op, op.Off, touched, op.Frame, h.frameSize)
					}
				}
			}
			switch v.Expect.Strict {
			case "ok", "corrupt", "truncated", "unsupported", "limit":
			default:
				t.Errorf("strict %q is not an errClass value", v.Expect.Strict)
			}
			e := v.Expect
			switch v.Class {
			case 1:
				if e.FirstDamage != 0 || e.TailFrom != nil || e.TailBase != "" {
					t.Error("class 1 must not set firstDamage, tailFrom or tailBase")
				}
				if e.Frames <= 0 {
					t.Error("class 1 needs frames")
				}
				switch e.PCM {
				case "intact", "prefix":
				case "extra":
					if len(v.Ops) != 1 || v.Ops[0].Op != "copy-frames" {
						t.Error("pcm extra needs exactly one copy-frames op")
					}
				case "offset":
					if len(v.Ops) != 1 || v.Ops[0].Op != "drop-frames" || v.Ops[0].Frame != 0 {
						t.Error("pcm offset needs exactly one drop-frames op at frame 0")
					}
				default:
					t.Errorf("class 1 pcm %q, want intact, prefix, extra or offset", e.PCM)
				}
				switch {
				case e.FFmpeg < 0 || e.MPG123 < 0: // 超大标签变体不测参考
					if !v.InTestOnly {
						t.Error("negative reference length on a variant that is written out")
					}
				case e.FFmpeg == e.MPG123:
					if e.Frames != e.FFmpeg {
						t.Errorf("frames %d, but both references give %d", e.Frames, e.FFmpeg)
					}
				case v.Note == "":
					t.Errorf("references disagree (ffmpeg %d, mpg123 %d) without a note", e.FFmpeg, e.MPG123)
				case e.Frames != e.FFmpeg && e.Frames != e.MPG123:
					t.Errorf("frames %d matches neither reference (ffmpeg %d, mpg123 %d)", e.Frames, e.FFmpeg, e.MPG123)
				}
			case 2:
				if e.Frames != 0 || e.PCM != "" {
					t.Error("class 2 must not set frames or pcm")
				}
				// omitempty 使 0 与缺省无法区分,所以第一帧损坏不能用 firstDamage 表示
				if e.FirstDamage <= 0 || e.FirstDamage > len(baseStarts) {
					t.Errorf("firstDamage %d, want 1..%d (the base's audio frames)", e.FirstDamage, len(baseStarts))
				}
				tailFrames := len(baseStarts)
				if e.TailBase != "" {
					if !exists(e.TailBase) {
						t.Fatalf("tailBase %s missing", e.TailBase)
					}
					tailData, tailStarts, _ := variantBase(t, e.TailBase)
					tailFrames = len(tailStarts)
					spf, ch, _ := frameShape(t, base, baseStarts)
					if tspf, tch, _ := frameShape(t, tailData, tailStarts); tspf != spf || tch != ch {
						t.Errorf("tailBase has %d samples per frame and %d channels, base has %d and %d", tspf, tch, spf, ch)
					}
				}
				if tf := e.TailFrom; tf != nil {
					if *tf < 0 || *tf >= tailFrames {
						t.Errorf("tailFrom %d, want 0..%d (the tail base's audio frames)", *tf, tailFrames-1)
					}
					// 省略 tailBase 表示就地损坏:尾部在损坏所在的帧之后
					if e.TailBase == "" && *tf <= e.FirstDamage {
						t.Errorf("tailFrom %d not after firstDamage %d", *tf, e.FirstDamage)
					}
				}
			default:
				t.Fatalf("class %d, want 1 or 2", v.Class)
			}

			skipHugeInShort(t, v)
			got := variantBytes(t, v)
			if bytes.Equal(got, base) {
				t.Error("variant equals its base")
			}
			if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != v.SHA256 {
				t.Errorf("variant bytes changed (sha256 %x, manifest %q): re-measure the reference lengths with referenceTools.lengthCommands, then update expect and sha256", sum, v.SHA256)
			}
		})
	}
}

// 写出全部变体(超大标签的除外)供 refgen 生成参考。包路径要写在 -args 之前:
// go test 把不认识的标志连同其后的参数都交给测试程序,包路径放在后面会被当成它的参数。
//
//	go test -count=1 -run '^TestWriteVariants$' . -args -write-variants DIR
func TestWriteVariants(t *testing.T) {
	if *writeVariants == "" {
		t.Skip("-write-variants not set")
	}
	if err := os.MkdirAll(*writeVariants, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range loadVariants(t) {
		if v.InTestOnly {
			continue
		}
		if err := os.WriteFile(filepath.Join(*writeVariants, v.Name+".mp3"), variantBytes(t, v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
