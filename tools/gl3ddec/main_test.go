package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunWritesF64LE(t *testing.T) {
	data, err := os.ReadFile("../../corpus/synthetic/short-tone-stereo-44k1-cbr192.mp3")
	if err != nil {
		t.Skip("corpus file not available:", err)
	}
	cases := []struct {
		name      string
		tail      int // 音频之后追加的零字节数
		wantClean bool
	}{
		{"clean", 0, true},
		// 默认模式是宽松的:音频之后的垃圾不使解码失败,只让 clean 为假
		{"garbage tail", 300, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := filepath.Join(dir, "tone.mp3")
			if err := os.WriteFile(in, append(data[:len(data):len(data)], make([]byte, tc.tail)...), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "tone.f64le")
			s, err := run(in, out)
			if err != nil {
				t.Fatal(err)
			}
			// 3 帧 × 1152 − enc_delay 576 − enc_padding 675 = 2205
			if s.Frames != 2205 || s.DecodedFrames != 3456 || !s.Trimmed || s.Clean != tc.wantClean || s.Channels != 2 || s.SampleRate != 44100 {
				t.Fatalf("summary %+v, want Clean=%v", s, tc.wantClean)
			}
			fi, err := os.Stat(out)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Size() != 2205*2*8 {
				t.Fatalf("output %d bytes, want %d", fi.Size(), 2205*2*8)
			}
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(b, &fields); err != nil {
				t.Fatal(err)
			}
			if clean, ok := fields["clean"].(bool); !ok || clean != tc.wantClean {
				t.Fatalf(`JSON "clean" = %v, want %v: %s`, fields["clean"], tc.wantClean, b)
			}
		})
	}
}
