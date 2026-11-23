package main

// RPC 控制面。
//
// 三套接口共用同一个引擎,互相之间没有状态:
//   - POST /rpc             JSON-RPC 2.0,方法名对齐 aria2(tellStatus/addUri/...)
//   - /api/tasks/...        REST 资源式接口,给自建 GUI / 脚本用
//   - GET  /events          SSE,推送引擎事件(实时进度)
//
// 安全:默认只监听 127.0.0.1。设置了 --rpc-secret 后,所有接口都需要
// Authorization: Bearer <token>;EventSource 这类无法设置请求头的浏览器 API
// 可以改用 ?token= 查询参数。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// rpcServer 是控制面 HTTP 服务。
type rpcServer struct {
	m    *Manager
	srv  *http.Server
	ln   net.Listener
	url  string
	auth string
	// cancel 用于 shutdown 方法:收到后取消引擎上下文,进程退出。
	cancel context.CancelFunc
}

// rpcRequest 是 JSON-RPC 2.0 请求体。
type rpcRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id,omitempty"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params,omitempty"`
}

// rpcResponse 是 JSON-RPC 2.0 响应体。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError 是 JSON-RPC 错误对象。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// startRPC 启动控制面。listen 为空时用 127.0.0.1:6800,"0" 表示随机端口。
func startRPC(ctx context.Context, m *Manager, listen, secret string, cancel context.CancelFunc) (*rpcServer, error) {
	addr := strings.TrimSpace(listen)
	switch addr {
	case "":
		addr = "127.0.0.1:6800"
	case "0":
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	s := &rpcServer{m: m, ln: ln, auth: secret, cancel: cancel}
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", s.handleRPC)
	mux.HandleFunc("/events", s.handleSSE)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/", s.handleTask)
	mux.HandleFunc("/metrics", metricsHandler(s))
	registerWebUI(mux, s.authorized, s.deny)
	mux.HandleFunc("/", s.handleRoot)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = s.srv.Close()
	}()
	s.url = fmt.Sprintf("http://%s", ln.Addr().String())
	return s, nil
}

// Close 关闭控制面服务。
func (s *rpcServer) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// authorized 校验访问令牌。未配置 --rpc-secret 时一律放行。
func (s *rpcServer) authorized(r *http.Request) bool {
	if s.auth == "" {
		return true
	}
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		if strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) == s.auth {
			return true
		}
	}
	return r.URL.Query().Get("token") == s.auth
}

// deny 未授权时返回 401。
func (s *rpcServer) deny(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}

// handleRoot 是服务信息页。
func (s *rpcServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.authorized(r) {
		s.deny(w)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":    "gotorrent",
		"version": version,
		"endpoints": []string{
			"POST /rpc            JSON-RPC 2.0(aria2 兼容方法名)",
			"GET  /api/tasks      任务列表",
			"POST /api/tasks      添加任务",
			"GET  /api/tasks/{id} 单个任务",
			"DEL  /api/tasks/{id} 移除任务",
			"POST /api/tasks/{id}/pause | /resume",
			"GET  /events         SSE 实时事件流",
			"GET  /metrics        Prometheus 指标",
			"GET  /ui             Web 控制面板",
		},
	})
}

// ---------------------------------------------------------------------------
// JSON-RPC
// ---------------------------------------------------------------------------

func (s *rpcServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		s.deny(w)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "解析请求失败: " + err.Error()}})
		return
	}
	result, err := s.dispatch(req.Method, req.Params)
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if err != nil {
		resp.Error = &rpcError{Code: -32000, Message: err.Error()}
	} else {
		resp.Result = result
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// dispatch 按方法名分发。方法名对齐 aria2 的 XML-RPC/JSON-RPC 命名,
// 便于直接对接 Sonarr / Radarr / 自研脚本。
func (s *rpcServer) dispatch(method string, params []json.RawMessage) (any, error) {
	switch method {
	case "addUri":
		uri := paramString(params, 0)
		if uri == "" {
			return nil, fmt.Errorf("addUri 需要磁力链接或种子文件路径")
		}
		return s.addTask(uri, paramOptions(params, 1))
	case "addTorrent":
		// aria2 兼容:第 1 个参数是 base64 编码的 .torrent 内容
		b64 := paramString(params, 0)
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("addTorrent 的 torrent 数据不是合法 base64: %w", err)
		}
		path, err := writeTempTorrent(raw)
		if err != nil {
			return nil, err
		}
		return s.addTask(path, paramOptions(params, 1))
	case "tellStatus":
		id := paramString(params, 0)
		st, err := s.m.Status(id)
		if err != nil {
			return nil, err
		}
		return aria2Status(st), nil
	case "tellActive", "tellWaiting", "tellStopped":
		var out []any
		for _, st := range s.m.List() {
			isActive := st.State == string(stateDownloading) || st.State == string(stateSeeding)
			// aria2 的 tellWaiting 同时包含"排队中"和"已暂停"的下载,
			// 靠 status 字段区分("waiting" / "paused")。
			isWaiting := st.State == string(stateQueued) || st.State == string(stateWaiting) || st.State == string(statePaused)
			isStopped := st.State == string(stateComplete) || st.State == string(stateError)
			var match bool
			switch method {
			case "tellActive":
				match = isActive
			case "tellWaiting":
				match = isWaiting
			case "tellStopped":
				match = isStopped
			}
			if match {
				out = append(out, aria2Status(st))
			}
		}
		if out == nil {
			out = []any{}
		}
		return out, nil
	case "pause":
		if err := s.m.Pause(paramString(params, 0)); err != nil {
			return nil, err
		}
		return paramString(params, 0), nil
	case "unpause", "forceActive":
		if err := s.m.Resume(paramString(params, 0)); err != nil {
			return nil, err
		}
		return paramString(params, 0), nil
	case "remove", "removeDownloadResult":
		id := paramString(params, 0)
		if err := s.m.Remove(id, paramBool(params, 1)); err != nil {
			return nil, err
		}
		return id, nil
	case "getVersion":
		return map[string]string{"version": version}, nil
	case "getGlobalStat":
		var total, completed int64
		active, waiting := 0, 0
		for _, st := range s.m.List() {
			total += st.Total
			completed += st.Completed
			switch st.State {
			case string(stateDownloading), string(stateSeeding):
				active++
			case string(stateQueued), string(stateWaiting):
				waiting++
			}
		}
		return map[string]any{
			"downloadSpeed":   fmt.Sprintf("%d", int64(0)),
			"numActive":       fmt.Sprintf("%d", active),
			"numWaiting":      fmt.Sprintf("%d", waiting),
			"numStopped":      fmt.Sprintf("%d", 0),
			"totalLength":     fmt.Sprintf("%d", total),
			"completedLength": fmt.Sprintf("%d", completed),
		}, nil
	case "shutdown":
		go func() {
			time.Sleep(200 * time.Millisecond)
			if s.cancel != nil {
				s.cancel()
			}
		}()
		return "shutdown requested", nil
	default:
		return nil, fmt.Errorf("不支持的方法: %s", method)
	}
}

// addTask 添加任务并返回其 ID。
func (s *rpcServer) addTask(input string, opts map[string]any) (string, error) {
	spec := taskSpec{Input: input}
	if v, ok := opts["dir"].(string); ok && v != "" {
		// 单任务目录:与全局 --dir 冲突时以任务级为准。
		if err := os.MkdirAll(v, 0o755); err == nil {
			opt.dir = v
		}
	}
	if v, ok := opts["out"].(string); ok {
		spec.Out = v
	}
	if v, ok := opts["select-file"].(string); ok {
		spec.Select = v
	}
	if v, ok := opts["select-filter"].(string); ok {
		spec.SelectFilter = v
	}
	if v, ok := opts["limit-rate"].(string); ok {
		spec.LimitRate = v
	}
	if v, ok := opts["max-upload-limit"].(string); ok {
		spec.UploadRate = v
	}
	if v, ok := opts["tag"].(string); ok {
		spec.Tag = v
	}
	return s.m.Add(spec)
}

// aria2Status 把内部快照转成 aria2 的字段命名(数值用字符串,与 aria2 一致)。
func aria2Status(st jsonStatus) map[string]any {
	status := "waiting"
	switch st.State {
	case string(stateDownloading), string(stateSeeding):
		status = "active"
	case string(statePaused):
		status = "paused"
	case string(stateComplete):
		status = "complete"
	case string(stateError):
		status = "error"
	}
	return map[string]any{
		"gid":             st.ID,
		"status":          status,
		"totalLength":     fmt.Sprintf("%d", st.Total),
		"completedLength": fmt.Sprintf("%d", st.Completed),
		"downloadSpeed":   fmt.Sprintf("%d", int64(st.DownloadSpeed)),
		"uploadSpeed":     fmt.Sprintf("%d", int64(st.UploadSpeed)),
		"infoHash":        st.InfoHash,
		"numPieces":       "0",
		"pieceLength":     "0",
		"files":           []any{},
		"dir":             opt.dir,
		"errorCode":       "0",
		"errorMessage":    st.Error,
	}
}

// ---------------------------------------------------------------------------
// REST
// ---------------------------------------------------------------------------

// handleTasks 处理 /api/tasks 的 GET(列表)与 POST(添加)。
func (s *rpcServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.deny(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.m.List())
	case http.MethodPost:
		var body struct {
			URI     string         `json:"uri"`
			Options map[string]any `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
			return
		}
		if body.URI == "" {
			writeJSONError(w, http.StatusBadRequest, "缺少 uri")
			return
		}
		id, err := s.addTask(body.URI, body.Options)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]string{"id": id})
	default:
		http.Error(w, "方法不支持", http.StatusMethodNotAllowed)
	}
}

// handleTask 处理 /api/tasks/{id} 及其子路径。
func (s *rpcServer) handleTask(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.deny(w)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		s.handleTasks(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch action {
	case "pause":
		if err := s.m.Pause(id); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]string{"id": id, "state": string(statePaused)})
	case "resume":
		if err := s.m.Resume(id); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]string{"id": id, "state": string(stateDownloading)})
	case "":
		switch r.Method {
		case http.MethodGet:
			st, err := s.m.Status(id)
			if err != nil {
				writeJSONError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, st)
		case http.MethodDelete:
			del := r.URL.Query().Get("deleteFiles") == "true"
			if err := s.m.Remove(id, del); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, map[string]string{"id": id, "state": "removed"})
		default:
			http.Error(w, "方法不支持", http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

// handleSSE 推送引擎事件。连接建立时先推一份全量快照,客户端可以立刻画界面。
func (s *rpcServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.deny(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前连接不支持流式响应", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	for _, st := range s.m.List() {
		writeSSE(w, "snapshot", st)
	}
	flusher.Flush()

	ch, unsub := s.m.Subscribe()
	defer unsub()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, string(ev.kind), ev.task)
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

// ---------------------------------------------------------------------------
// 参数解析辅助
// ---------------------------------------------------------------------------

func paramString(params []json.RawMessage, i int) string {
	if i >= len(params) {
		return ""
	}
	var s string
	if err := json.Unmarshal(params[i], &s); err == nil {
		return s
	}
	return ""
}

func paramBool(params []json.RawMessage, i int) bool {
	if i >= len(params) {
		return false
	}
	var b bool
	if err := json.Unmarshal(params[i], &b); err == nil {
		return b
	}
	return false
}

// paramOptions 解析第 i 个参数里的选项对象。
func paramOptions(params []json.RawMessage, i int) map[string]any {
	out := map[string]any{}
	if i >= len(params) {
		return out
	}
	if err := json.Unmarshal(params[i], &out); err != nil {
		return map[string]any{}
	}
	return out
}

// writeTempTorrent 把 base64 的种子内容落到临时文件,返回路径。
func writeTempTorrent(raw []byte) (string, error) {
	dir := filepath.Join(mustAbs(opt.dir), ".gotorrent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	f, err := os.CreateTemp(dir, "rpc-*.torrent")
	if err != nil {
		return "", fmt.Errorf("创建临时种子文件失败: %w", err)
	}
	name := f.Name()
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("写入临时种子文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("关闭临时种子文件失败: %w", err)
	}
	return name, nil
}
