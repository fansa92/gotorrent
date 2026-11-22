package main

// --stream:边下边播。
//
// anacrolix/torrent 的 File.NewReader() 会阻塞到所读分片可用,并自带
// readahead 把读取位置附近的分片提权,正好适合做流式播放。这里把它包成
// 一个本地 HTTP 服务,交给 http.ServeContent 处理 Range / 206 / HEAD,
// 播放器(VLC、PotPlayer、mpv)直接打开打印出来的地址即可。
//
// 注意:
//   - torrent.Reader 明确声明"Not safe for concurrent use",所以每个请求
//     各建一个 Reader,互不共享;
//   - Reader 只实现 io.ReadSeekCloser(没有 ReadAt),ServeContent 正好只要
//     io.ReadSeeker,匹配;
//   - 播放器可能在下载完成前反复 seek,未到的分片会让 Read 等待,这是预期行为。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/anacrolix/torrent"
)

type streamServer struct {
	ln   net.Listener
	srv  *http.Server
	url  string
	file *torrent.File
}

// startStream 在 127.0.0.1:port 上启动流服务,port=0 时随机分配。
func startStream(parent context.Context, f *torrent.File, port int) (*streamServer, error) {
	// 文件若被 -s / --select-filter 排除,Priority 为 None,Reader 会永远
	// 等待一个不会被请求的分片。串流意味着"这个文件我要",这里兜底置为 Normal。
	if f.Priority() == torrent.PiecePriorityNone {
		f.SetPriority(torrent.PiecePriorityNormal)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		rd := f.NewReader()
		defer rd.Close()

		// 客户端断开或进程退出时,让阻塞中的 Read 尽快返回。
		rctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			select {
			case <-parent.Done():
				cancel()
			case <-rctx.Done():
			}
		}()
		rd.SetContext(rctx)
		rd.SetReadahead(8 << 20)

		// name 用于 ServeContent 按扩展名推断 MIME,不要自己覆盖 Content-Type。
		http.ServeContent(w, r, f.DisplayPath(), time.Time{}, rd)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "gotorrent stream\nfile: %s\nsize: %d\nurl:  /stream\n", f.DisplayPath(), f.Length())
	})

	s := &streamServer{ln: ln, file: f, srv: &http.Server{Handler: mux}}
	go func() { _ = s.srv.Serve(ln) }()
	s.url = fmt.Sprintf("http://%s/stream", ln.Addr().String())
	return s, nil
}

func (s *streamServer) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// pickStreamFile 选出要串流的文件:
//  1. --stream-file 显式指定(编号或 glob)
//  2. 第一个被选中(要下载)的文件
//  3. 单文件种子的唯一文件
//
// 多文件且未指定时给出提示,避免用户以为串错了。
func pickStreamFile(tk *task, selected map[int]bool) (*torrent.File, error) {
	files := tk.t.Files()
	if spec := opt.streamFile; spec != "" {
		idx := filesBySpec(tk.t, spec)
		for i, f := range files {
			if idx[i+1] {
				return f, nil
			}
		}
		return nil, fmt.Errorf("--stream-file %q 没有匹配任何文件", spec)
	}
	for i, f := range files {
		if selected[i+1] {
			if len(files) > 1 {
				fmt.Fprintf(out, "[流] 多文件种子,默认串流第一个选中文件: %s(可用 --stream-file 指定)\n", f.DisplayPath())
			}
			return f, nil
		}
	}
	if len(files) == 1 {
		return files[0], nil
	}
	return nil, errors.New("没有可串流的选中文件")
}
