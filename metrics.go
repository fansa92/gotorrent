package main

// Prometheus 指标端点(/metrics)。
//
// 不引入 prometheus/client_golang(依赖树太重),直接手写 Prometheus 文本
// 暴露格式(OpenMetrics 之前的 text/plain version=0.0.4)。指标数据每请求从
// 引擎快照实时汇总,无需额外采样协程。
//
// 指标命名遵循 Prometheus 规范:<namespace>_<subsystem>_<name>_<unit>:
//   gotorrent_tasks{state=...}            各状态任务数
//   gotorrent_task_bytes{...}             任务已下载/上传字节
//   gotorrent_task_bytes_total{...}       累计
//   gotorrent_task_speed_bytes_per_second 下载/上传速率
//   gotorrent_task_peers                  活跃对等点数
//   gotorrent_task_seeders                连接的做种数
//   gotorrent_download_bytes_total        全局累计下载字节
//   gotorrent_upload_bytes_total          全局累计上传字节
//   gotorrent_engine_uptime_seconds       引擎运行时长

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// 全局累计字节计数,由引擎在采样点累加(跨请求单调递增)。
var (
	metricsDownloadTotal atomic.Int64
	metricsUploadTotal   atomic.Int64
	metricsStart         = time.Now()
)

// metricsHandler 返回 /metrics 处理器。复用控制面的鉴权逻辑。
func metricsHandler(s *rpcServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s != nil && !s.authorized(r) {
			s.deny(w)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var sb strings.Builder
		tasks := s.m.List()

		// 各状态任务计数
		stateCount := map[string]int{}
		var dlSpeed, upSpeed int64
		var totalBytes, completedBytes int64
		var peers, seeders int
		for _, st := range tasks {
			stateCount[st.State]++
			dlSpeed += int64(st.DownloadSpeed)
			upSpeed += int64(st.UploadSpeed)
			totalBytes += st.Total
			completedBytes += st.Completed
			peers += st.Peers
			seeders += st.Seeders
			writeGauge(&sb, "gotorrent_task_completed_bytes",
				"已下载字节(按选中文件)", st.InfoHash, st.Name,
				map[string]string{"state": st.State}, st.Completed)
			writeGauge(&sb, "gotorrent_task_download_speed_bytes_per_second",
				"任务下载速率", st.InfoHash, st.Name, nil, int64(st.DownloadSpeed))
			writeGauge(&sb, "gotorrent_task_upload_speed_bytes_per_second",
				"任务上传速率", st.InfoHash, st.Name, nil, int64(st.UploadSpeed))
			writeGauge(&sb, "gotorrent_task_peers",
				"活跃对等点数", st.InfoHash, st.Name, nil, int64(st.Peers))
			writeGauge(&sb, "gotorrent_task_seeders",
				"连接的做种数", st.InfoHash, st.Name, nil, int64(st.Seeders))
		}
		// tasks 状态计数
		states := make([]string, 0, len(stateCount))
		for k := range stateCount {
			states = append(states, k)
		}
		sort.Strings(states)
		sb.WriteString("# HELP gotorrent_tasks 各状态任务数\n# TYPE gotorrent_tasks gauge\n")
		for _, st := range states {
			writeGauge(&sb, "gotorrent_tasks", "各状态任务数", "", "",
				map[string]string{"state": st}, int64(stateCount[st]))
		}

		sb.WriteString("# HELP gotorrent_download_bytes_total 累计下载字节\n# TYPE gotorrent_download_bytes_total counter\n")
		fmt.Fprintf(&sb, "gotorrent_download_bytes_total %d\n", metricsDownloadTotal.Load())
		sb.WriteString("# HELP gotorrent_upload_bytes_total 累计上传字节\n# TYPE gotorrent_upload_bytes_total counter\n")
		fmt.Fprintf(&sb, "gotorrent_upload_bytes_total %d\n", metricsUploadTotal.Load())
		sb.WriteString("# HELP gotorrent_engine_uptime_seconds 引擎运行时长\n# TYPE gotorrent_engine_uptime_seconds gauge\n")
		fmt.Fprintf(&sb, "gotorrent_engine_uptime_seconds %.0f\n", time.Since(metricsStart).Seconds())
		sb.WriteString("# HELP gotorrent_download_speed_bytes_per_second 全局下载速率\n# TYPE gotorrent_download_speed_bytes_per_second gauge\n")
		fmt.Fprintf(&sb, "gotorrent_download_speed_bytes_per_second %d\n", dlSpeed)
		sb.WriteString("# HELP gotorrent_upload_speed_bytes_per_second 全局上传速率\n# TYPE gotorrent_upload_speed_bytes_per_second gauge\n")
		fmt.Fprintf(&sb, "gotorrent_upload_speed_bytes_per_second %d\n", upSpeed)
		sb.WriteString("# HELP gotorrent_total_bytes 计划下载总字节\n# TYPE gotorrent_total_bytes gauge\n")
		fmt.Fprintf(&sb, "gotorrent_total_bytes %d\n", totalBytes)
		sb.WriteString("# HELP gotorrent_completed_bytes 已完成字节\n# TYPE gotorrent_completed_bytes gauge\n")
		fmt.Fprintf(&sb, "gotorrent_completed_bytes %d\n", completedBytes)
		sb.WriteString("# HELP gotorrent_peers 全局对等点数\n# TYPE gotorrent_peers gauge\n")
		fmt.Fprintf(&sb, "gotorrent_peers %d\n", peers)
		sb.WriteString("# HELP gotorrent_seeders 全局做种数\n# TYPE gotorrent_seeders gauge\n")
		fmt.Fprintf(&sb, "gotorrent_seeders %d\n", seeders)

		_, _ = w.Write([]byte(sb.String()))
	}
}

// writeGauge 输出一条 gauge。可选 label(额外标签),id/name 为任务标识。
func writeGauge(sb *strings.Builder, name, help, id, name2 string, extra map[string]string, val int64) {
	_ = name2
	fmt.Fprintf(sb, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	labels := []string{}
	if id != "" {
		labels = append(labels, fmt.Sprintf("infohash=%q", id))
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		labels = append(labels, fmt.Sprintf("%s=%q", k, extra[k]))
	}
	if len(labels) == 0 {
		fmt.Fprintf(sb, "%s %d\n", name, val)
	} else {
		fmt.Fprintf(sb, "%s{%s} %d\n", name, strings.Join(labels, ","), val)
	}
}
