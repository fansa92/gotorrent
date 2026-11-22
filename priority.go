package main

// --priority-high:指定哪些文件优先下载(rtorrent 的 + 键 / qBittorrent 的
// 文件优先级高)。
//
// anacrolix/torrent 的文件优先级只有 None / Normal / High 三档有稳定语义,
// 其余(Readahead / Next / Now)是 Reader 内部使用的瞬时值,不适合做用户选项。
// 因此这里实现的是三档中的 "优先 / 普通 / 跳过":
//   --priority-high 命中  -> PiecePriorityHigh
//   被选中的其余文件      -> PiecePriorityNormal
//   未选中的文件          -> PiecePriorityNone(-s / --select-filter 决定)

import (
	"strings"

	"github.com/anacrolix/torrent"
)

// looksNumericSpec 判断选择串是否只由编号、区间、逗号、空白组成。
func looksNumericSpec(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && r != ',' && r != '-' && r != ' ' {
			return false
		}
	}
	return true
}

// filesBySpec 把 "1,3-5" 或 "*.mp4" 解析成 1-based 文件编号集合。
// 编号语法交给 parseSelect(支持 a-b 区间与 0=全部),其余按 glob 处理。
func filesBySpec(t *torrent.Torrent, spec string) map[int]bool {
	files := t.Files()
	out := map[int]bool{}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return out
	}
	if looksNumericSpec(spec) {
		sel := parseSelect(spec)
		if sel.all {
			for i := range files {
				out[i+1] = true
			}
			return out
		}
		for n := range sel.set {
			if n >= 1 && n <= len(files) {
				out[n] = true
			}
		}
		return out
	}
	f := parseFilterSpec(spec)
	for i, file := range files {
		p := file.DisplayPath()
		if f.match(p) && !f.excluded(p) {
			out[i+1] = true
		}
	}
	return out
}

// applyHighPriority 把 highSpec 命中的文件提到高优先级,其余被选中文件保持普通。
// 返回命中的文件数,便于在 0 命中时提示用户。
// 注意:高优先级文件一定是"要下载"的,所以会把 selected 里对应项补上。
func applyHighPriority(t *torrent.Torrent, highSpec string, selected map[int]bool) int {
	high := filesBySpec(t, highSpec)
	if len(high) == 0 {
		return 0
	}
	n := 0
	for i, f := range t.Files() {
		idx := i + 1
		switch {
		case high[idx]:
			f.SetPriority(torrent.PiecePriorityHigh)
			selected[idx] = true
			n++
		case selected[idx]:
			f.SetPriority(torrent.PiecePriorityNormal)
		}
	}
	return n
}
