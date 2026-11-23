package main

// Web UI 控制面板:嵌入式单页(embed.FS),复用控制面的 REST / SSE 接口。
//
// 路由:GET /ui 返回 index.html;/ui/assets/... 提供静态资源。UI 通过
// /api/tasks(REST)与 /events(SSE)与引擎交互,鉴权与控制面一致(Bearer token
// 或 ?token=)。--webui=false 可关闭(默认开启,仅 daemon 模式挂载)。

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed webui
var webuiFS embed.FS

// registerWebUI 把 Web UI 挂到 mux。authorized 为控制面的鉴权函数。
func registerWebUI(mux *http.ServeMux, authorized func(*http.Request) bool, deny func(http.ResponseWriter)) {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		return
	}
	fileServer := http.FileServer(http.FS(sub))

	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			deny(w)
			return
		}
		// 从 embed 读取 index.html(ServeFile 走磁盘路径,嵌入后不可用)
		b, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "Web UI 资源缺失", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			deny(w)
			return
		}
		// 去掉 /ui 前缀交给 FileServer
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/ui")
		fileServer.ServeHTTP(w, r)
	})
}
