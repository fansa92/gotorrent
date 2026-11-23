package main

// TUI 交互界面:附加到正在运行的守护进程,用键盘管理任务。
//
// 设计:
//   - 与 Web UI 同源,复用守护进程的控制面 REST 接口(/api/tasks)与鉴权,
//     通过 --rpc-listen/--rpc-secret 连到已启动的 gotorrent --daemon。
//   - 标准库 HTTP + ANSI 渲染;终端原始模式按平台隔离到 tui_term_*.go,
//     Windows 10+ 控制台开启 VT 处理后直接识别 ANSI 转义序列。
//   - 键位:↑/↓ 选择 · p 暂停 · r 继续 · d 删除 · q 退出 · R 刷新。

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

func runTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: gotorrent tui [选项]")
		fmt.Fprintln(os.Stderr, "交互式终端界面,附加到运行中的守护进程管理任务。")
		fmt.Fprintln(os.Stderr, "选项:")
		fs.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\n键位: ↑/↓ 选择 · p 暂停 · r 继续 · d 删除 · q 退出 · R 刷新")
	}
	fs.StringVar(&opt.rpcListen, "rpc-listen", "127.0.0.1:6800", "守护进程控制面地址")
	fs.StringVar(&opt.rpcSecret, "rpc-secret", opt.rpcSecret, "控制面令牌(与守护进程 --rpc-secret 一致)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	t := &tuiClient{
		base:   "http://" + normalizeHostPort(opt.rpcListen),
		secret: opt.rpcSecret,
		hc:     &http.Client{Timeout: 4 * time.Second},
	}
	if err := t.run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI: %v\n", err)
		os.Exit(1)
	}
}

// normalizeHostPort 把监听地址转成可用于拨号的 host:port;缺省端口补 6800。
func normalizeHostPort(addr string) string {
	if addr == "" {
		return "127.0.0.1:6800"
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr + ":6800"
}

// taskRow 是界面里的一行任务(源自 /api/tasks 的 jsonStatus)。
type taskRow struct {
	ID      string
	Name    string
	State   string
	Total   int64
	Done    int64
	DL, UL  float64
	Peers   int
	Seeders int
	ETA     float64
	Tag     string
}

func (r taskRow) progress() float64 {
	if r.Total <= 0 {
		return 0
	}
	return float64(r.Done) / float64(r.Total)
}

// ---------------------------------------------------------------------------
// 控制面客户端
// ---------------------------------------------------------------------------

type tuiClient struct {
	base   string
	secret string
	hc     *http.Client

	// 界面状态:选中行、任务快照、底部提示信息与最近一次拉取错误。
	cursor int
	rows   []taskRow
	msg    string
	err    error
}

func (c *tuiClient) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("%s: %s", resp.Status, msg)
	}
	return data, nil
}

// list 拉取任务快照。
func (c *tuiClient) list() ([]taskRow, error) {
	data, err := c.do(http.MethodGet, "/api/tasks", nil)
	if err != nil {
		return nil, err
	}
	var sts []jsonStatus
	if err := json.Unmarshal(data, &sts); err != nil {
		return nil, err
	}
	rows := make([]taskRow, 0, len(sts))
	for _, st := range sts {
		rows = append(rows, taskRow{
			ID: st.ID, Name: st.Name, State: st.State, Total: st.Total, Done: st.Completed,
			DL: st.DownloadSpeed, UL: st.UploadSpeed, Peers: st.Peers, Seeders: st.Seeders,
			ETA: st.ETA, Tag: st.Tag,
		})
	}
	return rows, nil
}

func (c *tuiClient) pause(id string) error {
	_, err := c.do(http.MethodPost, "/api/tasks/"+id+"/pause", nil)
	return err
}
func (c *tuiClient) resume(id string) error {
	_, err := c.do(http.MethodPost, "/api/tasks/"+id+"/resume", nil)
	return err
}
func (c *tuiClient) remove(id string) error {
	_, err := c.do(http.MethodDelete, "/api/tasks/"+id, nil)
	return err
}

// add 通过磁力链接或种子路径添加任务。
func (c *tuiClient) add(uri string) error {
	_, err := c.do(http.MethodPost, "/api/tasks", map[string]any{"uri": uri})
	return err
}

// ---------------------------------------------------------------------------
// ANSI 序列(平台无关)
// ---------------------------------------------------------------------------

const (
	ansiHideCursor = "\x1b[?25l"
	ansiShowCursor = "\x1b[?25h"
	ansiClear      = "\x1b[H\x1b[2J"
)

// 终端原始模式的进入/退出实现见 tui_term_windows.go / tui_term_other.go。

// ---------------------------------------------------------------------------
// 主循环
// ---------------------------------------------------------------------------

func (t *tuiClient) run() error {
	st, err := enableVT()
	if err != nil {
		return err
	}
	defer st.restore()

	// 输入:原始模式逐字节读,ANSI 转义序列解析方向键。
	keyCh := make(chan keyEvent, 8)
	go readKeys(os.Stdin, keyCh)

	t.cursor = 0
	t.rows, t.err = t.list()
	t.render()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev, ok := <-keyCh:
			if !ok {
				return t.err // stdin 关闭
			}
			quit := t.handleKey(ev)
			if quit {
				return nil
			}
		case <-ticker.C:
			t.rows, t.err = t.list()
		}
		// 主动作后立即刷新一次,让用户马上看到状态变化。
		t.render()
	}
}

// handleKey 处理一个按键,返回是否应退出。
func (t *tuiClient) handleKey(ev keyEvent) bool {
	switch ev.kind {
	case keyQuit:
		return true
	case keyUp:
		if t.cursor > 0 {
			t.cursor--
		}
	case keyDown:
		if t.cursor < len(t.rows)-1 {
			t.cursor++
		}
	case keyRefresh:
		t.rows, t.err = t.list()
	case keyPause, keyResume, keyRemove, keyAdd:
		t.act(ev.kind)
	}
	return false
}

// act 对当前选中的任务执行暂停/继续/删除;添加则读一行 URI。
func (t *tuiClient) act(kind keyKind) {
	t.msg = ""
	if kind == keyAdd {
		// 退出原始模式读一行输入,读完再恢复。
		fmt.Print(ansiShowCursor)
		var uri string
		fmt.Print("\x1b[H\x1b[2J输入磁力链接或种子路径(回车确认): ")
		if _, err := fmt.Scanln(&uri); err == nil && uri != "" {
			if err := t.add(uri); err != nil {
				t.msg = "添加失败: " + err.Error()
			} else {
				t.msg = "已添加: " + uri
			}
		}
		fmt.Print(ansiHideCursor)
		return
	}
	if len(t.rows) == 0 {
		t.msg = "没有任务"
		return
	}
	if t.cursor < 0 || t.cursor >= len(t.rows) {
		return
	}
	id := t.rows[t.cursor].ID
	var err error
	switch kind {
	case keyPause:
		err = t.pause(id)
	case keyResume:
		err = t.resume(id)
	case keyRemove:
		err = t.remove(id)
	}
	if err != nil {
		t.msg = "失败: " + err.Error()
	} else {
		t.rows, t.err = t.list()
	}
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

type keyKind int

const (
	keyUp keyKind = iota
	keyDown
	keyPause
	keyResume
	keyRemove
	keyAdd
	keyQuit
	keyRefresh
	keyOther
)

type keyEvent struct {
	kind keyKind
	ch   byte
}

// readKeys 从 stdin 读原始字节并解析成 keyEvent。ANSI 方向键是 3 字节序列。
func readKeys(f *os.File, ch chan<- keyEvent) {
	defer close(ch)
	buf := make([]byte, 3)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		b := buf[:n]
		switch {
		case n == 0:
			continue
		case b[0] == 'q' || b[0] == 'Q':
			ch <- keyEvent{kind: keyQuit}
		case b[0] == 'p':
			ch <- keyEvent{kind: keyPause}
		case b[0] == 'r':
			ch <- keyEvent{kind: keyResume}
		case b[0] == 'd':
			ch <- keyEvent{kind: keyRemove}
		case b[0] == 'a':
			ch <- keyEvent{kind: keyAdd}
		case b[0] == 'R':
			ch <- keyEvent{kind: keyRefresh}
		case b[0] == 0x1b && n >= 3 && b[1] == '[':
			switch b[2] {
			case 'A':
				ch <- keyEvent{kind: keyUp}
			case 'B':
				ch <- keyEvent{kind: keyDown}
			}
		default:
			ch <- keyEvent{kind: keyOther, ch: b[0]}
		}
	}
}

func (t *tuiClient) render() {
	var sb strings.Builder
	sb.WriteString(ansiClear)
	sb.WriteString("gotorrent TUI — p 暂停 · r 继续 · d 删除 · a 添加 · R 刷新 · q 退出\n")
	if t.err != nil {
		sb.WriteString("⚠ 拉取任务失败: " + t.err.Error() + " (守护进程在运行吗?)\n")
	}
	if t.msg != "" {
		sb.WriteString("› " + t.msg + "\n")
	}
	if len(t.rows) == 0 {
		sb.WriteString("\n(暂无任务;按 a 添加)\n")
		fmt.Print(sb.String())
		return
	}
	for i, r := range t.rows {
		cursor := "  "
		if i == t.cursor {
			cursor = "▸ "
		}
		prog := fmt.Sprintf("%5.1f%%", r.progress()*100)
		bar := renderBar(r.progress(), 20)
		eta := "—"
		if r.State == "downloading" && r.ETA > 0 {
			eta = etaStr(r.ETA)
		}
		name := truncateName(r.Name, 32)
		tag := ""
		if r.Tag != "" {
			tag = " [" + r.Tag + "]"
		}
		fmt.Fprintf(&sb, "%s%-8s %s %s %s ↓%-8s ↑%-8s p%-3d s%-3d eta:%-6s%s\n",
			cursor, r.State, prog, bar,
			name, humanBps(int64(r.DL)), humanBps(int64(r.UL)),
			r.Peers, r.Seeders, eta, tag)
	}
	// 光标移到当前行(标题行数 + msg 行数 + 索引),保持 ANSI 全屏刷新。
	fmt.Print(sb.String())
}

func renderBar(p float64, width int) string {
	filled := int(p * float64(width))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", width-filled) + "]"
}

func truncateName(s string, n int) string {
	r := []rune(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
