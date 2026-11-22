package main

// --select-filter:按扩展名 / glob 选文件,比记编号方便。
// 支持逗号分隔多个模式、"!" 前缀排除;匹配不区分大小写,同时匹配
// 完整相对路径(分隔符 /)与文件名,所以 "*.mp4" 能匹配任意层级下的 mp4。

import (
	"path"
	"strings"

	"github.com/anacrolix/torrent"
)

type filterRule struct {
	pattern string
	exclude bool
}

type fileFilter struct {
	rules []filterRule
	set   bool // 是否指定了 --select-filter
}

func parseFilterSpec(s string) fileFilter {
	f := fileFilter{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		r := filterRule{}
		if strings.HasPrefix(part, "!") {
			r.exclude = true
			part = strings.TrimSpace(strings.TrimPrefix(part, "!"))
		}
		if part == "" {
			continue
		}
		r.pattern = strings.ToLower(strings.ReplaceAll(part, "\\", "/"))
		f.rules = append(f.rules, r)
	}
	f.set = len(f.rules) > 0
	return f
}

func (f fileFilter) ruleMatches(r filterRule, displayPath string) bool {
	p := strings.ToLower(strings.ReplaceAll(displayPath, "\\", "/"))
	base := path.Base(p)
	if ok, err := path.Match(r.pattern, p); err == nil && ok {
		return true
	}
	if ok, err := path.Match(r.pattern, base); err == nil && ok {
		return true
	}
	if !strings.Contains(r.pattern, "/") {
		// "*.mp4" 这类纯扩展名模式,补一次后缀匹配(兜底 path.Match 的 / 语义)
		return strings.HasSuffix(base, strings.TrimPrefix(r.pattern, "*"))
	}
	return false
}

// match 只判断"包含规则"是否命中;排除规则统一在 selectedSet 里做二次剔除,
// 这样 --select=1 与 --select-filter='!*.nfo' 组合时也能生效。
func (f fileFilter) match(displayPath string) bool {
	if !f.set {
		return false
	}
	for _, r := range f.rules {
		if !r.exclude && f.ruleMatches(r, displayPath) {
			return true
		}
	}
	return false
}

func (f fileFilter) excluded(displayPath string) bool {
	for _, r := range f.rules {
		if r.exclude && f.ruleMatches(r, displayPath) {
			return true
		}
	}
	return false
}

func (f fileFilter) hasInclude() bool {
	for _, r := range f.rules {
		if !r.exclude {
			return true
		}
	}
	return false
}

// selectedSet 返回被选中的文件编号(1-based)。
// 规则:都没指定 → 全部;只有编号 → 按编号;只有 glob → 按 glob;
// 两者都有 → 并集;只有 !排除规则 → 全部起步再剔除;最后统一应用 ! 规则。
func selectedSet(t *torrent.Torrent, selSpec, filterSpec string) map[int]bool {
	files := t.Files()
	sel := parseSelect(selSpec)
	filter := parseFilterSpec(filterSpec)
	out := map[int]bool{}
	switch {
	case sel.all && !filter.set, // 无任何选择规则:全选
		sel.all && filter.set && !filter.hasInclude(): // 只有排除规则:全选后剔除
		for i := range files {
			out[i+1] = true
		}
	default:
		if !sel.all {
			for n := range sel.set {
				if n >= 1 && n <= len(files) {
					out[n] = true
				}
			}
		}
		if filter.set {
			for i, f := range files {
				if filter.match(f.DisplayPath()) {
					out[i+1] = true
				}
			}
		}
	}
	if filter.set {
		for i, f := range files {
			if filter.excluded(f.DisplayPath()) {
				delete(out, i+1)
			}
		}
	}
	return out
}

// applySelectionSet 按已算好的 1-based 编号集合应用选择。
// 选中的 f.Download()(Normal),其余 f.Cancel()(None)。
func applySelectionSet(t *torrent.Torrent, set map[int]bool) bool {
	files := t.Files()
	n := 0
	for i, f := range files {
		if set[i+1] {
			f.Download()
			n++
		} else {
			f.Cancel()
		}
	}
	return n > 0
}

// applySelection 应用选择并校验至少有一个文件被选中。
// 单文件 torrent 且 -s 1 / -s 0 时沿用全量的兼容行为。
func applySelection(t *torrent.Torrent, selSpec, filterSpec string) bool {
	files := t.Files()
	filter := parseFilterSpec(filterSpec)
	if len(files) == 1 && !filter.set && (selSpec == "1" || selSpec == "0" || selSpec == "") {
		downloadAllFiles(files)
		return true
	}
	return applySelectionSet(t, selectedSet(t, selSpec, filterSpec))
}
