package main

// 存储目录归类:把 --dir-by-type 与 --tag 映射到 anacrolix/torrent 的
// 自定义 TorrentDirMaker,让数据落到 <base>/<category|tag>/。
//
// Client 只有一个 storage 实例,pathMaker 拿不到具体 task,所以这里用
// infohash -> 归类目标的映射表。addInput 阶段(拿到 infohash 后)登记映射,
// activate/OpenTorrent 时 storage 用它决定子目录。
//
// 优先级:任务级 tag(--tag) > 文件类型归类(--dir-by-type) > 无归类(平铺)。

import (
	"path/filepath"
	"sync"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type classifyEntry struct {
	byType bool
	tag    string
}

type classifyRegistry struct {
	mu   sync.Mutex
	m    map[metainfo.Hash]classifyEntry
	base string
}

var registry = &classifyRegistry{m: map[metainfo.Hash]classifyEntry{}}

// registerClassify 登记一个 infohash 的归类目标。
func registerClassify(h metainfo.Hash, e classifyEntry) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.m[h] = e
}

// unregisterClassify 任务移除时清理。
func unregisterClassify(h metainfo.Hash) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.m, h)
}

// classifyPathMaker 返回 storage 的 TorrentDirMaker。
// base 为基础下载目录;命中登记则返回 <base>/<subdir>,否则 <base>。
func classifyPathMaker() func(baseDir string, info *metainfo.Info, infoHash metainfo.Hash) string {
	return func(baseDir string, info *metainfo.Info, infoHash metainfo.Hash) string {
		registry.mu.Lock()
		e, ok := registry.m[infoHash]
		registry.mu.Unlock()
		sub := ""
		if ok {
			if e.tag != "" {
				sub = sanitizePathSeg(e.tag)
			} else if e.byType && info != nil {
				var paths []string
				var sizes []int64
				for _, f := range info.UpvertedFiles() {
					paths = append(paths, filepath.Join(f.Path...))
					sizes = append(sizes, f.Length)
				}
				sub = sanitizePathSeg(dominantCategory(paths, sizes))
			}
		}
		if sub == "" {
			return baseDir
		}
		return joinDir(baseDir, sub)
	}
}

// classifyStorage 用归类 pathMaker 构建存储(带断点续传 completion)。
func classifyStorage(baseDir string, completion storage.PieceCompletion) storage.ClientImplCloser {
	return storage.NewFileWithCustomPathMakerAndCompletion(
		baseDir, classifyPathMaker(), completion,
	)
}
