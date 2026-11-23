package main

// 标签与目录自动归类。
//
// 两件事:
//  1. 标签(--tag / -i 行内 tag=xxx):给任务打标签,用于筛选、目录归类、
//     控制面过滤。标签存进 taskSpec.Tag,随会话恢复。
//  2. 目录归类(--dir-by-type):激活任务时按文件类型把数据存到
//     <dir>/<type>/<原名>,如 movie.torrent -> downloads/video/movie/。
//
// 归类规则(按扩展名映射,大小写不敏感):
//   video: mp4 mkv avi mov wmv flv webm m4v ts mpg mpeg rmvb
//   audio: mp3 flac wav aac ogg m4a wma ape
//   image: jpg jpeg png gif bmp webp svg heic
//   doc:   pdf doc docx xls xlsx ppt pptx txt md epub mobi
//   archive: zip rar 7z tar gz bz2 xz iso
//   app:   exe msi dmg pkg deb rpm apk appimage
//   其他  -> other
//
// 多文件种子:按任务内最大文件的类型决定归类目录(整任务存到同一类目录)。

import (
	"path/filepath"
	"strings"
)

// typeCategories 扩展名 -> 类别。全部小写,不含点。
var typeCategories = map[string]string{}

func init() {
	add := func(cat string, exts ...string) {
		for _, e := range exts {
			typeCategories[e] = cat
		}
	}
	add("video", "mp4", "mkv", "avi", "mov", "wmv", "flv", "webm", "m4v", "ts", "mpg", "mpeg", "rmvb", "vob")
	add("audio", "mp3", "flac", "wav", "aac", "ogg", "m4a", "wma", "ape", "opus")
	add("image", "jpg", "jpeg", "png", "gif", "bmp", "webp", "svg", "heic", "tiff")
	add("doc", "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "md", "epub", "mobi", "azw3")
	add("archive", "zip", "rar", "7z", "tar", "gz", "bz2", "xz", "iso", "cab")
	add("app", "exe", "msi", "dmg", "pkg", "deb", "rpm", "apk", "appimage", "bin")
	add("subtitle", "srt", "ass", "ssa", "sub", "idx")
}

// fileCategory 返回文件名的类别。
func fileCategory(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if cat, ok := typeCategories[ext]; ok {
		return cat
	}
	return "other"
}

// dominantCategory 返回文件列表的主导类别:按总字节数最大的文件类型决定。
// 用于多文件种子归类(整任务进同一目录)。
func dominantCategory(paths []string, sizes []int64) string {
	if len(paths) == 0 {
		return "other"
	}
	best := "other"
	var bestSize int64
	seen := map[string]int64{}
	for i, p := range paths {
		cat := fileCategory(p)
		sz := int64(0)
		if i < len(sizes) {
			sz = sizes[i]
		}
		seen[cat] += sz
		if seen[cat] > bestSize {
			bestSize = seen[cat]
			best = cat
		}
	}
	return best
}

// classifyDir 计算归类后的子目录:<base>/<category>。
// sanitize 标签防止路径穿越(标签可能来自不可信 torrent 元数据)。
func classifyDir(base, category string) string {
	cat := sanitizePathSeg(category)
	return filepath.Join(mustAbs(base), cat)
}

// sanitizePathSeg 清洗路径段,禁止 ..、绝对路径、控制字符。
func sanitizePathSeg(s string) string {
	s = strings.TrimSpace(s)
	// 去掉非法字符
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", "_")
	}
	if out == "" || out == "." {
		return "other"
	}
	return out
}

// joinDir 在 base 下安全地拼 subdir(对 subdir 做清洗)。
func joinDir(base, sub string) string {
	return filepath.Join(mustAbs(base), sanitizePathSeg(sub))
}
