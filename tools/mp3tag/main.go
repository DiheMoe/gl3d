// Command mp3tag 解析 MP3 文件的首个帧头、ID3 标签与 Xing/Info/VBRI/LAME 元数据,
// 以 JSON 输出,供登记语料清单条目。任一文件解析失败时退出码为 1。
//
// 用法:mp3tag FILE...
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/DiheMoe/gl3d/tools/internal/mp3info"
)

type record struct {
	File string `json:"file"`
	*mp3info.Info
	Error string `json:"error,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mp3tag FILE...")
		os.Exit(2)
	}
	failed := false
	records := make([]record, 0, len(os.Args)-1)
	for _, path := range os.Args[1:] {
		rec := record{File: path}
		data, err := os.ReadFile(path)
		if err == nil {
			rec.Info, err = mp3info.Parse(data)
		}
		if err != nil {
			rec.Error = err.Error()
			failed = true
		}
		records = append(records, rec)
	}
	out, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mp3tag:", err)
		os.Exit(2)
	}
	fmt.Println(string(out))
	if failed {
		os.Exit(1)
	}
}
