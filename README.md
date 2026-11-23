# gotorrent

基于 [anacrolix/torrent](https://github.com/anacrolix/torrent) 的 BitTorrent 下载工具。既有 aria2 风格的命令行体验，也提供守护进程 + JSON-RPC / REST / Web UI 控制面。

## 特性

- **下载核心**：磁力链接与 `.torrent`、DHT / PEX / WebSeed / Tracker、断点续传（bbolt 记录已校验分片）、哈希校验、顺序下载、做种分享率与时长上限。
- **任务编排**：批量任务文件（`-i`）、并发队列（`--max-concurrent-downloads`）、会话恢复（`--resume`）、每任务独立限速、文件编号或 glob 选择（`--select-filter`）、文件优先级（`--priority-high`）。
- **自动化**：RSS 订阅自动下载、分时段限速调度、完成钩子（含 `GOTRENT_*` 环境变量）、完成历史（`gotorrent history`）。
- **控制面**：守护模式下提供 JSON-RPC 2.0（方法名对齐 aria2）、REST API、SSE 事件流、Web UI、指标端点，以及 TUI 客户端（`gotorrent tui`）。
- **输出与日志**：mpb 进度条（非 TTY 自动降级为逐行可读进度）、`--json` 纯 NDJSON 输出、结构化日志（text/json、轮转）。
- **网络**：SOCKS5 / HTTP 代理、协议加密三档、追加 Tracker、IP 黑名单、IPv6 / UPnP 开关。
- **部署**：静态交叉编译，提供 Dockerfile、docker-compose、systemd 安装脚本。

## 安装

### 下载预编译包

从 [Releases](../../releases) 下载对应平台的压缩包，解压后即可运行。命名格式：

```
gotorrent_<版本>_<系统>_<架构>.tar.gz   # Linux / macOS / FreeBSD
gotorrent_<版本>_<系统>_<架构>.zip      # Windows
```

提供的架构：`amd64`、`arm64`、`386`（仅 Linux）、`armv7` / `armv6`（仅 Linux）。Windows 提供 `amd64` 与 `arm64`（32 位 Windows 因依赖库限制不提供）。

### 源码编译

需要 Go 1.26.8 或更新版本：

```bash
git clone https://github.com/fansa92/gotorrent.git
cd gotorrent
go build -o gotorrent .
```

### Docker

```bash
docker build -t gotorrent:local .
docker compose up -d
```

## 快速上手

```bash
# 下载 .torrent 或磁力链接
gotorrent ubuntu.torrent
gotorrent -d D:\data "magnet:?xt=urn:btih:xxxxxxxx"

# 先看文件清单，再按编号 / glob 选文件
gotorrent --all big.torrent
gotorrent -s 2,4 --seed=false big.torrent
gotorrent --select-filter "*.mp4,!*sample*" big.torrent

# 限速、做种上限、速度不足自动退出
gotorrent --limit-rate 2M --max-upload-limit 512K \
  --seed-ratio 1.0 --stall-timeout 120 big.torrent

# 批量任务 + 并发 3 + JSON 输出（stdout 纯 NDJSON，人读信息走 stderr）
gotorrent -i tasks.txt --max-concurrent-downloads 3 --json > status.ndjson

# 中断后一键续传（任务参数自动还原）
gotorrent --resume

# 边下边播：播放器打开 http://127.0.0.1:8080/stream
gotorrent --stream --sequential --seed=false movie.torrent

# 磁力链补私有 tracker / 强制加密 / 关键文件优先
gotorrent --tracker udp://tracker.example:1337/announce "magnet:?xt=urn:btih:..."
gotorrent --encryption required --priority-high "*.mp4" big.torrent

# 制作种子
gotorrent create -t udp://tracker.opentrackr.org:1337/announce ./myfile
```

`-i` 批量文件每行一个任务，可带行内参数（只作用于该任务）：

```
magnet:?xt=urn:btih:aaaa... select-filter=*.mkv limit-rate=2M
D:\torrents\movie.torrent out=movie.mkv select=1,3
```

## 守护模式与控制面

```bash
# 启动常驻服务
gotorrent --daemon --rpc-listen 0.0.0.0:6800 --rpc-secret change-me

# Web UI
# http://127.0.0.1:6800/ui

# JSON-RPC（aria2 风格方法名）
curl -s http://127.0.0.1:6800/rpc -H 'Authorization: Bearer change-me' \
  -d '{"jsonrpc":"2.0","id":1,"method":"addUri","params":[["magnet:?xt=urn:btih:..."]]}'

# REST
curl -s http://127.0.0.1:6800/api/tasks -H 'Authorization: Bearer change-me'
curl -s -X POST http://127.0.0.1:6800/api/tasks/<id>/pause -H 'Authorization: Bearer change-me'

# SSE 事件流 / 指标
# GET /events        GET /metrics

# 终端客户端（附加到守护进程）
gotorrent tui --rpc-listen 127.0.0.1:6800 --rpc-secret change-me
```

RPC 方法：`addUri` / `addTorrent` / `tellStatus` / `tellActive` / `tellWaiting` / `tellStopped` / `pause` / `unpause` / `remove` / `getVersion` / `getGlobalStat` / `shutdown`。

## 配置与状态

| 路径 | 作用 |
|---|---|
| `~/.gotorrent/config` | `key=value` 配置文件（`#` 注释）；命令行显式参数优先 |
| `~/.gotorrent/session.json` | 未完成任务及参数，`--resume` 读取 |
| `<下载目录>/.gotorrent/state.db` | 分片校验状态，断点续传 |
| `<下载目录>/.gotorrent/<infohash>.torrent` | 磁力元数据缓存 |
| `<配置文件>.seen` | RSS 已处理条目记录 |

配置文件示例：

```ini
dir = D:/downloads
seed = false
limit-rate = 5M
max-concurrent-downloads = 3
select-filter = *.mp4,!*sample*
schedule.1 = 09:00-23:00,1M,256K
schedule.2 = 23:00-09:00,0,0
```

运行 `gotorrent --help` 查看全部选项。

## 开发

```bash
go vet ./...
go test ./...
go build -o gotorrent .
```

交叉编译示例：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o gotorrent-linux-arm64 .
```

推送 `v*` 标签会触发 GitHub Actions：先跑 vet / test 与交叉编译，再为多个系统架构打包并发布 Release。

## 许可证

MIT
