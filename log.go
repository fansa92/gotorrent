package main

// 结构化日志系统(基于标准库 log/slog,零新增依赖)。
//
// 设计目标:把散落在各处的诊断输出(警告/错误/跳过/失败)统一到一条结构化
// 通道,便于生产环境接入日志采集(ELK / Loki / 云日志服务);同时保留终端
// 可读输出,不改变默认体验。
//
// 通道划分:
//   - 用户展示(out / mpb 进度条):任务进度、文件清单、重命名等,属于产品输出,
//     保持 fmt.Fprintf 原样,不走结构化日志。
//   - 结构化日志(slog):诊断类消息 --log-level/--log-format/--log-file 控制。
//     默认 level=info,format=text,输出到 stderr;可与终端提示并存。
//
// --json 模式:结构化日志默认仍走独立通道,不与 stdout 的 NDJSON 状态流混淆;
// 若把 --log-file 指到文件,可做到 stdout 纯状态、日志文件纯诊断。

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 日志选项(由命令行 / 配置文件注入)。
type logOptions struct {
	level  string // debug|info|warn|error
	format string // text|json
	file   string // 日志文件路径;空则只写 stderr
	rotate int64  // 单文件大小上限(字节),>0 时按大小轮转,0 不轮转
	keep   int    // 轮转保留的历史文件数
}

var logOpt = logOptions{
	level:  "info",
	format: "text",
	rotate: 0,
	keep:   7,
}

// logFileState 管理日志文件的轮转。slog 的 handler 绑定一个 io.Writer,
// 我们用一个会自动轮转的 writer 包装,handler 无需感知轮转。
type logFileState struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	keep    int
	f       *os.File
	size    int64
	rotateN int
}

// Write 实现 io.Writer,写入后检查大小并按需轮转。
func (s *logFileState) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		if err := s.openLocked(); err != nil {
			return 0, err
		}
	}
	n, err := s.f.Write(p)
	s.size += int64(n)
	if err == nil && s.maxSize > 0 && s.size >= s.maxSize {
		s.rotateLocked()
	}
	return n, err
}

func (s *logFileState) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	s.f = f
	s.size = st.Size()
	return nil
}

// rotateLocked 关闭当前文件,重命名带序号,再开新文件。
// 历史文件形如 gotorrent.log.1 / gotorrent.log.2 ...(1 为最新),超出 keep 删除最旧。
func (s *logFileState) rotateLocked() {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
	s.rotateN++
	// 先把现有 .1 -> .2 ... 依次后移,腾出 .1 给当前文件
	for i := s.keep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", s.path, i)
		dst := fmt.Sprintf("%s.%d", s.path, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	if s.keep > 0 {
		_ = os.Rename(s.path, fmt.Sprintf("%s.1", s.path))
	} else {
		_ = os.Rename(s.path, fmt.Sprintf("%s.%d", s.path, time.Now().Unix()))
	}
	// 删除超出保留数量的旧文件
	if s.keep > 0 {
		for i := s.keep + 1; i <= s.keep+20; i++ {
			p := fmt.Sprintf("%s.%d", s.path, i)
			if _, err := os.Stat(p); err == nil {
				_ = os.Remove(p)
			}
		}
	}
	_ = s.openLocked()
}

func (s *logFileState) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		err := s.f.Close()
		s.f = nil
		return err
	}
	return nil
}

// 日志全局状态
var (
	slogger   *slog.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	logFile                = &logFileState{}
	logInited bool
)

// setupLogging 根据当前 logOpt 构建 slog.Logger。幂等,可在测试里反复调用。
func setupLogging() {
	var writers []io.Writer
	writers = append(writers, os.Stderr)
	if logOpt.file != "" {
		logFile.mu.Lock()
		logFile.path = mustAbs(logOpt.file)
		logFile.maxSize = logOpt.rotate
		logFile.keep = logOpt.keep
		logFile.mu.Unlock()
		writers = append(writers, logFile)
	}
	var w io.Writer = io.MultiWriter(writers...)

	var level slog.Level
	switch strings.ToLower(logOpt.level) {
	case "debug":
		level = slog.LevelDebug
	case "info", "":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.EqualFold(logOpt.format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	slogger = slog.New(h)
	logInited = true
}

// closeLogging 关闭日志文件(若有)。
func closeLogging() {
	if logInited {
		_ = logFile.Close()
	}
}

// bootstrapLogging 在 parseFlags 之前,先用配置文件里的默认值初始化日志,
// 让早期错误(非法 flag、config 读取)也能走结构化通道。
func bootstrapLogging() {
	cfg := loadConfigFile()
	logOpt.level = cfgStr(cfg, "log-level", "info")
	logOpt.format = cfgStr(cfg, "log-format", "text")
	logOpt.file = cfgStr(cfg, "log-file", "")
	if r := cfgStr(cfg, "log-rotate", ""); r != "" {
		if n, err := parseSize(r); err == nil {
			logOpt.rotate = n
		}
	}
	logOpt.keep = cfgInt(cfg, "log-keep", 7)
	setupLogging()
}

// applyLogOptions 在 parseFlags 之后,用命令行显式值覆盖并重建 logger。
func applyLogOptions() {
	logOpt.level = opt.logLevel
	logOpt.format = opt.logFormat
	logOpt.file = opt.logFile
	if opt.logRotate != "" {
		if n, err := parseSize(opt.logRotate); err == nil {
			logOpt.rotate = n
		}
	}
	logOpt.keep = opt.logKeep
	setupLogging()
}

// ---------------------------------------------------------------------------
// 诊断类消息的统一入口。
// 既写结构化日志(slog),也保留终端可读前缀,与项目现有 "[警告] ..." 风格一致。
// 用户展示类消息(进度/清单)不走这里,直接 fmt.Fprintf(out, ...)。
// ---------------------------------------------------------------------------

func logDebug(format string, a ...any) {
	slogger.Debug(fmt.Sprintf(format, a...))
}

func logInfo(format string, a ...any) {
	slogger.Info(fmt.Sprintf(format, a...))
}

func logWarn(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	slogger.Warn(msg)
	if !opt.jsonOutput {
		fmt.Fprintf(os.Stderr, "[警告] %s\n", msg)
	}
}

func logError(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	slogger.Error(msg)
	if !opt.jsonOutput {
		fmt.Fprintf(os.Stderr, "[错误] %s\n", msg)
	}
}

// logTaskErr 带任务上下文的错误日志。
func logTaskErr(id, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	slogger.Error(msg, "task_id", id)
	if !opt.jsonOutput {
		fmt.Fprintf(os.Stderr, "[错误] %s\n", msg)
	}
}

// setDefaultLogger 把全局 slog 默认 logger 设为当前实例(供需要默认 logger 的库使用)。
func setDefaultLogger() {
	slog.SetDefault(slogger)
}
