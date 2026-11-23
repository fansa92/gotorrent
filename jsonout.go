package main

// --json:每任务一行 NDJSON 状态,stdout 只输出 JSON,便于脚本与上游集成。
// 所有人读信息(头部、汇总、提示、警告)改走 stderr。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// out 是人读输出的目标。普通模式为 stdout;--json 时切到 stderr,
// 保证 stdout 是纯 NDJSON 流。
//
// 声明成 io.Writer 而不是 *os.File,是为了让测试能把它换成 io.Discard,
// 以及让调用方可以接任意 Writer。
var out io.Writer = os.Stdout

type jsonFile struct {
	Index    int    `json:"index"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Selected bool   `json:"selected"`
}

type jsonStatus struct {
	Event         string     `json:"event"` // progress / complete / start / error / summary
	Time          string     `json:"time"`
	ID            string     `json:"id,omitempty"`         // 任务 ID(infohash)
	CreatedAt     string     `json:"created_at,omitempty"` // 任务创建时间
	Name          string     `json:"name,omitempty"`
	InfoHash      string     `json:"infohash,omitempty"`
	Input         string     `json:"input,omitempty"`
	State         string     `json:"state"`
	Total         int64      `json:"total"`
	Completed     int64      `json:"completed"`
	Progress      float64    `json:"progress"`
	DownloadSpeed float64    `json:"download_speed"`
	UploadSpeed   float64    `json:"upload_speed"`
	Peers         int        `json:"peers"`
	Seeders       int        `json:"seeders"`
	ETA           float64    `json:"eta_seconds"`
	Active        int        `json:"active"`
	Queued        int        `json:"queued"`
	Tag           string     `json:"tag,omitempty"`
	Error         string     `json:"error,omitempty"`
	Files         []jsonFile `json:"files,omitempty"`
	URL           string     `json:"url,omitempty"` // stream 事件的播放地址
}

func emitJSON(s jsonStatus) {
	if !opt.jsonOutput {
		return
	}
	s.Time = time.Now().Format(time.RFC3339)
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stdout, string(b))
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func emitError(input, msg string) {
	emitJSON(jsonStatus{Event: "error", State: "error", Input: input, Error: msg})
}

// -----------------------------------------------------------------------------
// -i 批量输入:每行一个任务,可在后面追加 tab/空格分隔的 key=value 参数
// -----------------------------------------------------------------------------

// parseInputFile 读取任务列表。
// 行格式: <magnet|torrent路径> [key=value ...]
// 支持 # 注释、空行、参数前缀 "--";参数键:
// out / select / s / select-filter / limit-rate / max-upload-limit
func parseInputFile(path string) ([]taskSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var specs []taskSpec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 支持带空格的路径:用双引号把第一个字段包起来,如
		// "D:\my videos\a.torrent" select-filter=*.mp4
		var fields []string
		if strings.HasPrefix(line, `"`) {
			if end := strings.Index(line[1:], `"`); end >= 0 {
				fields = append(fields, line[1:1+end])
				fields = append(fields, strings.Fields(line[2+end:])...)
			}
		}
		if len(fields) == 0 {
			fields = strings.Fields(line)
		}
		if len(fields) == 0 {
			continue
		}
		s := taskSpec{Input: fields[0]}
		for _, kv := range fields[1:] {
			k, v, ok := strings.Cut(strings.TrimPrefix(kv, "--"), "=")
			if !ok {
				return nil, fmt.Errorf("%s:%d 参数 %q 缺少 =value", path, lineNo, kv)
			}
			switch strings.ToLower(k) {
			case "out", "o":
				s.Out = v
			case "select", "s":
				s.Select = v
			case "select-filter":
				s.SelectFilter = v
			case "priority-high":
				s.PriorityHigh = v
			case "limit-rate":
				s.LimitRate = v
			case "max-upload-limit":
				s.UploadRate = v
			case "tag":
				s.Tag = v
			default:
				return nil, fmt.Errorf("%s:%d 未知参数 %q", path, lineNo, k)
			}
		}
		specs = append(specs, s)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return specs, nil
}
