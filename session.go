package main

// 会话恢复:.gotorrent/session.json 记录未完成任务及其参数,
// gotorrent --resume 一键继续,无需重新贴链接(aria2 session 风格)。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const sessionVersion = 1

// taskSpec 描述一个下载任务。每个任务带自己的参数(-i 文件行内参数 /
// --resume 恢复),全局 opt 只作为缺省值。
type taskSpec struct {
	Input        string `json:"input"`                   // magnet 或 .torrent 路径
	Out          string `json:"out,omitempty"`           // -o 重命名
	Select       string `json:"select,omitempty"`        // 按编号选择
	SelectFilter string `json:"select_filter,omitempty"` // 按 glob 选择
	PriorityHigh string `json:"priority_high,omitempty"` // 该任务优先下载的文件
	LimitRate    string `json:"limit_rate,omitempty"`    // 该任务独立下载限速
	UploadRate   string `json:"max_upload_limit,omitempty"`
	Paused       bool   `json:"paused,omitempty"` // 上次退出时该任务处于暂停状态
	Tag          string `json:"tag,omitempty"`    // 标签,用于分类与筛选
}

// sessionOptions 保存会影响到续传行为的全局参数;--resume 时用于补齐
// 那些"本次命令行没有显式指定"的选项。
type sessionOptions struct {
	Dir            string   `json:"dir"`
	Seed           bool     `json:"seed"`
	MaxConn        int      `json:"max_conn"`
	LimitRate      string   `json:"limit_rate"`
	UploadRate     string   `json:"max_upload_limit"`
	Select         string   `json:"select"`
	SelectFilter   string   `json:"select_filter"`
	Out            string   `json:"out"`
	MaxConcurrent  int      `json:"max_concurrent_downloads"`
	MetaTimeout    int      `json:"meta_timeout"`
	Sequential     bool     `json:"sequential"`
	Verify         bool     `json:"verify"`
	OnComplete     string   `json:"on_complete"`
	SeedRatio      float64  `json:"seed_ratio"`
	SeedTime       float64  `json:"seed_time"`
	StallTimeout   int      `json:"stall_timeout"`
	Proxy          string   `json:"proxy"`
	Blocklist      string   `json:"blocklist"`
	PublicTrackers bool     `json:"public_trackers"`
	EnableDHT      bool     `json:"enable_dht"`
	EnablePEX      bool     `json:"enable_pex"`
	EnableUTP      bool     `json:"enable_utp"`
	EnableHTTP     bool     `json:"enable_http_seed"`
	ListenPort     int      `json:"listen_port"`
	DisableIPv6    bool     `json:"disable_ipv6"`
	NoPortFwd      bool     `json:"no_port_forwarding"`
	NoEncrypt      bool     `json:"no_encrypt"`
	Encryption     string   `json:"encryption,omitempty"`
	Trackers       []string `json:"trackers,omitempty"`
	PriorityHigh   string   `json:"priority_high,omitempty"`
}

type sessionFile struct {
	Version int            `json:"version"`
	SavedAt time.Time      `json:"saved_at"`
	Options sessionOptions `json:"options"`
	Tasks   []taskSpec     `json:"tasks"`
}

// sessionPath 放在用户目录(与 ~/.gotorrent/config 同目录),而不是下载目录:
// 否则 `gotorrent -d D:\data <链接>` 之后直接 `gotorrent --resume` 会因为找不到
// 会话文件而失败,还得记住当时的 -d。恢复时由 sessionOptions.Dir(绝对路径)
// 自动还原下载目录,真正做到一键续传。
func sessionPath() string {
	if cfg := defaultConfigPath(); cfg != "" {
		return filepath.Join(filepath.Dir(cfg), "session.json")
	}
	return filepath.Join(mustAbs(opt.dir), ".gotorrent", "session.json")
}

// legacySessionPath 兼容早期放在下载目录里的会话文件(只读,便于平滑升级)。
func legacySessionPath() string {
	return filepath.Join(mustAbs(opt.dir), ".gotorrent", "session.json")
}

func snapshotOptions() sessionOptions {
	return sessionOptions{
		Dir:            mustAbs(opt.dir),
		Seed:           opt.seed,
		MaxConn:        opt.maxConn,
		LimitRate:      opt.limitRate,
		UploadRate:     opt.maxUploadRate,
		Select:         opt.selectFiles,
		SelectFilter:   opt.selectFilter,
		Out:            opt.out,
		MaxConcurrent:  opt.maxConcurrent,
		MetaTimeout:    opt.metaTimeout,
		Sequential:     opt.sequential,
		Verify:         opt.verify,
		OnComplete:     opt.onComplete,
		SeedRatio:      opt.seedRatio,
		SeedTime:       opt.seedTime,
		StallTimeout:   opt.stallTimeout,
		Proxy:          opt.proxy,
		Blocklist:      opt.blocklist,
		PublicTrackers: opt.publicTrackers,
		EnableDHT:      opt.enableDHT,
		EnablePEX:      opt.enablePEX,
		EnableUTP:      opt.enableUTP,
		EnableHTTP:     opt.enableHTTP,
		ListenPort:     opt.listenPort,
		DisableIPv6:    opt.disableIPv6,
		NoPortFwd:      opt.noPortFwd,
		NoEncrypt:      opt.noEncrypt,
		Encryption:     opt.encryption,
		Trackers:       append([]string(nil), opt.trackersCustom...),
		PriorityHigh:   opt.priorityHigh,
	}
}

// applySessionOptions 把上次会话的参数补进 opt,但不覆盖本次命令行显式指定的项。
func applySessionOptions(o sessionOptions) {
	e := explicitFlags()
	has := func(names ...string) bool {
		for _, n := range names {
			if e[n] {
				return true
			}
		}
		return false
	}
	if !has("dir", "d") && o.Dir != "" {
		opt.dir = o.Dir
	}
	if !has("seed") {
		opt.seed = o.Seed
	}
	if !has("max-conn") && o.MaxConn > 0 {
		opt.maxConn = o.MaxConn
	}
	if !has("limit-rate") {
		opt.limitRate = o.LimitRate
	}
	if !has("max-upload-limit") {
		opt.maxUploadRate = o.UploadRate
	}
	if !has("select", "s") {
		opt.selectFiles = o.Select
	}
	if !has("select-filter") {
		opt.selectFilter = o.SelectFilter
	}
	if !has("out", "o") {
		opt.out = o.Out
	}
	if !has("max-concurrent-downloads") && o.MaxConcurrent > 0 {
		opt.maxConcurrent = o.MaxConcurrent
	}
	if !has("meta-timeout") && o.MetaTimeout > 0 {
		opt.metaTimeout = o.MetaTimeout
	}
	if !has("sequential") {
		opt.sequential = o.Sequential
	}
	if !has("verify") {
		opt.verify = o.Verify
	}
	if !has("on-complete") {
		opt.onComplete = o.OnComplete
	}
	if !has("seed-ratio") {
		opt.seedRatio = o.SeedRatio
	}
	if !has("seed-time") {
		opt.seedTime = o.SeedTime
	}
	if !has("stall-timeout") && o.StallTimeout > 0 {
		opt.stallTimeout = o.StallTimeout
	}
	if !has("proxy") {
		opt.proxy = o.Proxy
	}
	if !has("blocklist") {
		opt.blocklist = o.Blocklist
	}
	if !has("public-trackers") {
		opt.publicTrackers = o.PublicTrackers
	}
	if !has("enable-dht") {
		opt.enableDHT = o.EnableDHT
	}
	if !has("enable-pex") {
		opt.enablePEX = o.EnablePEX
	}
	if !has("enable-utp") {
		opt.enableUTP = o.EnableUTP
	}
	if !has("enable-http-seed") {
		opt.enableHTTP = o.EnableHTTP
	}
	if !has("listen-port") && o.ListenPort > 0 {
		opt.listenPort = o.ListenPort
	}
	if !has("disable-ipv6") {
		opt.disableIPv6 = o.DisableIPv6
	}
	if !has("no-port-forwarding") {
		opt.noPortFwd = o.NoPortFwd
	}
	if !has("no-encrypt") {
		opt.noEncrypt = o.NoEncrypt
	}
	// 加密档位:--no-encrypt 视为已显式指定,不再被会话覆盖。
	if !has("encryption", "no-encrypt") && o.Encryption != "" {
		switch o.Encryption {
		case "required", "preferred", "tolerated":
			opt.encryption = o.Encryption
		}
	}
	if !has("tracker") && len(o.Trackers) > 0 {
		opt.trackersCustom = append([]string(nil), o.Trackers...)
	}
	if !has("priority-high") {
		opt.priorityHigh = o.PriorityHigh
	}
}

func loadSession() (sessionFile, error) {
	var s sessionFile
	b, err := os.ReadFile(sessionPath())
	if os.IsNotExist(err) {
		legacy := legacySessionPath()
		if legacy != sessionPath() {
			b, err = os.ReadFile(legacy)
		}
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("解析 %s 失败: %w", sessionPath(), err)
	}
	if s.Version != sessionVersion {
		return s, fmt.Errorf("会话文件版本不兼容(%d,期望 %d)", s.Version, sessionVersion)
	}
	return s, nil
}

// saveSession 原子写入(临时文件 + rename),避免中途退出留下半截 JSON。
func saveSession(tasks []taskSpec) error {
	if len(tasks) == 0 {
		err := os.Remove(sessionPath())
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	dir := filepath.Dir(sessionPath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s := sessionFile{
		Version: sessionVersion,
		SavedAt: time.Now(),
		Options: snapshotOptions(),
		Tasks:   tasks,
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "session-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, sessionPath())
}

// sessionWarn 把会话写入失败作为非致命警告(不影响下载)。
func sessionWarn(err error) {
	if err != nil {
		logWarn("更新会话文件失败: %v", err)
	}
}
