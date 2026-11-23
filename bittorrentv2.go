package main

// BitTorrent v2 (BEP 52) 种子制作支持。
//
// 下载侧:v2/hybrid 的握手协商(ExtensionBitV2Upgrade)、文件树读取、
// 分片校验由 anacrolix/torrent 内建自动处理,无需额外开关。
//
// 制作侧:库未暴露"从文件树构建 v2 info"的高层 API,这里按 BEP 52 规范实现:
//   - 每个文件按 16 KiB 块计算 SHA-256,自底向上构建 Merkle 树得到 pieces root
//   - 大于分片大小的文件需要 piece layers(中间层哈希,紧凑序列化)
//   - v2 要求文件按分片大小对齐(padding)
//   - 生成 hybrid(v1+v2,兼容老客户端)或 v2-only 两种 info
//
// 参考:https://www.bittorrent.org/beps/bep_0052.html

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anacrolix/torrent/merkle"
	"github.com/anacrolix/torrent/metainfo"
)

const (
	v2BlockSize   = 16 << 10 // 16 KiB,固定
	v2MetaVersion = 2
)

// fileInfoV2 是待制作文件的中立表示(相对路径用 / 分隔)。
type fileInfoV2 struct {
	relPath  string // 相对根目录,已转 /
	fullPath string
	length   int64
}

// buildV2FromPath 遍历 root 收集文件,构建 v2 info。
// hybrid=true 时同时生成 v1 pieces(分片对齐布局)。
// 返回 piece layers(BEP 52:整包 v2 infohash -> 各大文件 piece layer 顺序拼接)。
func buildV2FromPath(root string, hybrid bool) (*metainfo.Info, map[string]string, error) {
	infos, err := collectFilesV2(root)
	if err != nil {
		return nil, nil, err
	}
	total := int64(0)
	for _, f := range infos {
		total += f.length
	}

	info := &metainfo.Info{}
	info.Name = filepath.Base(root)
	info.PieceLength = choosePieceLengthV2(total)
	info.MetaVersion = v2MetaVersion

	// 构建 FileTree,同时收集 piece layers(BEP 52:key=文件 merkle root,value=层哈希拼接)。
	tree, pieceLayers, err := buildFileTree(root, infos, info.PieceLength)
	if err != nil {
		return nil, nil, err
	}
	info.FileTree = tree

	if hybrid {
		// v1 兼容字段:让 GeneratePieces 基于 FileTree 的分片对齐布局生成,
		// 保证 v1 pieces 与 v2 offsets 一致(v2 文件按分片对齐,padding 计入)。
		if err := info.GeneratePieces(func(fi metainfo.FileInfo) (io.ReadCloser, error) {
			return os.Open(filepath.Join(root, filepath.Join(fi.BestPath()...)))
		}); err != nil {
			return nil, nil, fmt.Errorf("生成 v1 分片哈希失败: %w", err)
		}
	}
	return info, pieceLayers, nil
}

// collectFilesV2 收集 root 下所有普通文件。
func collectFilesV2(root string) ([]fileInfoV2, error) {
	var out []fileInfoV2
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, fileInfoV2{
			relPath:  filepath.ToSlash(rel),
			fullPath: path,
			length:   fi.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].relPath < out[j].relPath })
	return out, nil
}

// buildFileTree 递归构建 FileTree,同时返回 piece layers(infohash hex -> 紧凑层哈希)。
func buildFileTree(root string, infos []fileInfoV2, pieceLen int64) (metainfo.FileTree, map[string]string, error) {
	type node struct {
		children map[string]*node
		file     *fileInfoV2
	}
	rootNode := &node{children: map[string]*node{}}
	for i := range infos {
		f := &infos[i]
		parts := strings.Split(f.relPath, "/")
		cur := rootNode
		for _, p := range parts[:len(parts)-1] {
			if cur.children[p] == nil {
				cur.children[p] = &node{children: map[string]*node{}}
			}
			cur = cur.children[p]
		}
		leaf := parts[len(parts)-1]
		cur.children[leaf] = &node{file: f}
	}

	pieceLayers := map[string]string{} // merkle root hex -> 该文件层哈希拼接(仅 >pieceLen 的文件)
	var build func(n *node) (*metainfo.FileTree, error)
	build = func(n *node) (*metainfo.FileTree, error) {
		ft := &metainfo.FileTree{}
		if n.file != nil {
			// 叶子:计算 pieces root(必要时记 piece layer)
			pr, layer, err := piecesRootFor(root, n.file, pieceLen)
			if err != nil {
				return nil, err
			}
			ft.File.Length = n.file.length
			ft.File.PiecesRoot = string(pr[:])
			if layer != "" {
				// BEP 52 / 库校验:piece layers 的 key 是 raw 32 字节 merkle root,
				// value 是 raw 字节的层哈希拼接(库 CompactLayerToSliceHashes 按 32B 切)。
				pieceLayers[string(pr[:])] = layer
			}
			return ft, nil
		}
		ft.Dir = map[string]metainfo.FileTree{}
		keys := make([]string, 0, len(n.children))
		for k := range n.children {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, err := build(n.children[k])
			if err != nil {
				return nil, err
			}
			ft.Dir[k] = *sub
		}
		return ft, nil
	}
	tree, err := build(rootNode)
	if err != nil {
		return metainfo.FileTree{}, nil, err
	}
	return *tree, pieceLayers, nil
}

// piecesRootFor 计算单个文件的 Merkle pieces root。
// 文件大于分片时返回紧凑 piece layer 供 piece layers 字段使用。
func piecesRootFor(root string, f *fileInfoV2, pieceLen int64) ([32]byte, string, error) {
	fh, err := os.Open(f.fullPath)
	if err != nil {
		return [32]byte{}, "", err
	}
	defer fh.Close()

	// 逐分片:每分片按 16KiB 块哈希,块数不足 2 的幂用 pad hash 补齐
	numPieces := (f.length + pieceLen - 1) / pieceLen
	var leafHashes [][32]byte
	buf := make([]byte, pieceLen)
	for i := int64(0); i < numPieces; i++ {
		n, err := io.ReadFull(fh, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return [32]byte{}, "", err
		}
		leafHashes = append(leafHashes, merkleRoot(buf[:n], v2BlockSize))
	}
	if len(leafHashes) == 0 {
		// 空文件:单个 pad hash
		pad := merkleRoot(nil, v2BlockSize)
		leafHashes = append(leafHashes, pad)
	}
	pr := merkle.RootWithPadHash(leafHashes, padHashForPiece(pieceLen))

	layer := ""
	if len(leafHashes) > 1 {
		layer = compactLayerBytes(leafHashes)
	}
	return pr, layer, nil
}

// merkleRoot 对一段数据按 blockSize 分块,自底向上建树返回 root。
func merkleRoot(data []byte, blockSize int) [32]byte {
	var hashes [][32]byte
	for i := 0; i < len(data); i += blockSize {
		end := i + blockSize
		if end > len(data) {
			end = len(data)
		}
		h := sha256.Sum256(data[i:end])
		hashes = append(hashes, h)
	}
	if len(hashes) == 0 {
		return sha256.Sum256(nil)
	}
	return merkle.Root(hashes)
}

// padHashForPiece 计算分片层的 padding hash(全 16KiB 空块的树根)。
func padHashForPiece(pieceLen int64) [32]byte {
	blocksPerPiece := pieceLen / v2BlockSize
	if blocksPerPiece < 1 {
		blocksPerPiece = 1
	}
	blockHashes := make([][32]byte, blocksPerPiece)
	return merkle.Root(blockHashes)
}

// compactLayerBytes 把一层哈希按 raw 字节顺序拼接(BEP 52 piece layers value)。
// 库的 CompactLayerToSliceHashes 按 32 字节切片,因此这里必须用 raw 字节而非 hex。
func compactLayerBytes(hashes [][32]byte) string {
	var b strings.Builder
	for _, h := range hashes {
		b.Write(h[:])
	}
	return b.String()
}

// v2FilesToV1 把文件列表转成 v1 Files(路径切分)。
func v2FilesToV1(infos []fileInfoV2) []metainfo.FileInfo {
	out := make([]metainfo.FileInfo, 0, len(infos))
	for _, f := range infos {
		out = append(out, metainfo.FileInfo{
			Path:   strings.Split(f.relPath, "/"),
			Length: f.length,
		})
	}
	return out
}

// choosePieceLengthV2 v2 推荐最小 16 KiB 块,分片大小选 2 的幂。
func choosePieceLengthV2(total int64) int64 {
	if total == 0 {
		return 16 << 10
	}
	pl := int64(16 << 10)
	for total/(pl*64) > 0 && pl < 16<<20 {
		pl *= 2
	}
	if pl < v2BlockSize {
		pl = v2BlockSize
	}
	return pl
}

func modeSuffix(v2, hybrid bool) string {
	if hybrid {
		return ", hybrid"
	}
	return ", v2-only"
}
