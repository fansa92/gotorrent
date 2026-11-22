package main

// 配置文件:~/.gotorrent/config,key=value 行,# 或 ; 为注释。
// 优先级:命令行显式参数 > 配置文件 > 内置默认值。
// 实现方式是"把配置值作为 flag 的默认值注册",flag.Parse() 天然让 CLI 覆盖。

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gotorrent", "config")
}

// loadConfigFile 读取配置文件;不存在时静默返回空配置。
func loadConfigFile() map[string]string {
	cfg := map[string]string{}
	path := defaultConfigPath()
	if path == "" {
		return cfg
	}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[配置] 读取 %s 失败: %v\n", path, err)
		}
		return cfg
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "[配置] 忽略无法解析的行: %s\n", line)
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(k), "--"))
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" {
			cfg[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[配置] 读取 %s 失败: %v\n", path, err)
	}
	return cfg
}

func cfgStr(cfg map[string]string, key, def string) string {
	if v, ok := cfg[key]; ok {
		return v
	}
	return def
}

func cfgBool(cfg map[string]string, key string, def bool) bool {
	if v, ok := cfg[key]; ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
		fmt.Fprintf(os.Stderr, "[配置] %s 布尔值无效: %q(已用默认值 %v)\n", key, v, def)
	}
	return def
}

func cfgInt(cfg map[string]string, key string, def int) int {
	if v, ok := cfg[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
		fmt.Fprintf(os.Stderr, "[配置] %s 整数无效: %q(已用默认值 %d)\n", key, v, def)
	}
	return def
}

func cfgFloat(cfg map[string]string, key string, def float64) float64 {
	if v, ok := cfg[key]; ok {
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return n
		}
		fmt.Fprintf(os.Stderr, "[配置] %s 数值无效: %q(已用默认值 %g)\n", key, v, def)
	}
	return def
}

// explicitFlags 返回命令行显式设置过的 flag 名(用于 --resume 时避免覆盖用户本次参数)。
func explicitFlags() map[string]bool {
	m := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { m[f.Name] = true })
	return m
}
