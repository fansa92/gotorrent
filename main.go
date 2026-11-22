// gotorrent - 基于 anacrolix/torrent 的 BitTorrent 下载工具(aria2 风格命令行)
//
// 用法:
//
//	gotorrent [选项] <torrent文件|magnet链接> ...
//
// 示例:
//
//	gotorrent ubuntu-24.04-desktop-amd64.iso.torrent
//	gotorrent "magnet:?xt=urn:btih:..."
//	gotorrent -d ./downloads --limit-rate 5M --seed=false file.torrent
//	gotorrent --all big.torrent            # 查看文件清单及编号
//	gotorrent --select=1,3-5 big.torrent   # 只下载第 1、3~5 个文件
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/iplist"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/vbauerster/mpb/v8"
	"golang.org/x/net/proxy"
)

var version = "1.0.0"

// 全局命令行选项(仿 aria2 参数命名)
type options struct {
	dir            string   // -d, --dir            下载目录
	out            string   // -o, --out            单文件重命名
	selectFiles    string   // -s, --select         选择文件编号
	all            bool     // --all                仅列出文件清单
	dryRun         bool     // --dry-run            仅显示任务信息
	hashOnly       bool     // --hash               仅计算 infohash
	seed           bool     // --seed               下载后做种
	maxConn        int      // --max-conn           每任务最大连接数
	limitRate      string   // --limit-rate         下载限速
	maxUploadRate  string   // --max-upload-limit   上传限速
	enableDHT      bool     // --enable-dht
	enablePEX      bool     // --enable-pex
	enableUTP      bool     // --enable-utp
	enableHTTP     bool     // --enable-http-seed   WebSeed
	listenPort     int      // --listen-port
	metaTimeout    int      // --meta-timeout       magnet 获取元数据超时(秒),<=0 不限
	verify         bool     // --verify             下载前强制重新校验已有数据
	stallTimeout   int      // --stall-timeout      连续无下载速度 N 秒后自动退出,0 禁用
	seedRatio      float64  // --seed-ratio         做种分享率上限,0 不限
	seedTime       float64  // --seed-time          做种时长上限(分钟),0 不限
	onComplete     string   // --on-complete        任务完成后执行的命令
	proxy          string   // --proxy              代理地址(socks5:// 或 http://)
	publicTrackers bool     // --public-trackers    magnet 自动附加公共 tracker
	sequential     bool     // --sequential         顺序下载(边下边播)
	disableIPv6    bool     // --disable-ipv6
	noPortFwd      bool     // --no-port-forwarding 禁用 UPnP/NAT-PMP 端口映射
	noEncrypt      bool     // --no-encrypt         禁用 BT 协议加密(--encryption 的别名)
	encryption     string   // --encryption         required|preferred|tolerated
	priorityHigh   string   // --priority-high      优先下载的文件(编号或 glob)
	trackersCustom []string // --tracker           追加 tracker(可重复)
	stream         bool     // --stream             边下边播
	streamPort     int      // --stream-port        流服务端口,0 随机
	streamFile     string   // --stream-file        指定串流文件
	blocklist      string   // --blocklist          IP 黑名单文件(P2P 格式)
	selectFilter   string   // --select-filter      按 glob 选文件
	jsonOutput     bool     // --json               每任务一行 NDJSON
	inputFile      string   // -i, --input-file     批量任务列表
	maxConcurrent  int      // --max-concurrent-downloads 同时下载数,0 不限
	resume         bool     // --resume             恢复 .gotorrent/session.json
	daemon         bool     // --daemon             守护模式:常驻运行并开放控制面
	rpcListen      string   // --rpc-listen         控制面监听地址,如 127.0.0.1:6800
	rpcSecret      string   // --rpc-secret         控制面令牌鉴权(Authorization: Bearer)
	rssFile        string   // --rss-file           RSS 订阅配置文件路径
	rssInterval    int      // --rss-interval       RSS 轮询间隔(分钟),0 关闭
}

var opt options

func main() {
	if len(os.Args) > 1 && os.Args[1] == "create" {
		runCreate(os.Args[2:])
		return
	}

	parseFlags()

	// 任务来源:--resume 会话文件 + -i 批量文件 + 命令行位置参数,按此顺序去重合并。
	var specs []taskSpec
	seen := map[string]bool{}
	addSpec := func(s taskSpec) {
		// 本地 .torrent 路径统一存绝对路径,保证 --resume 换工作目录后仍能找到文件
		if !isMagnet(s.Input) && !strings.Contains(s.Input, "://") {
			s.Input = mustAbs(s.Input)
		}
		if seen[s.Input] {
			return
		}
		seen[s.Input] = true
		specs = append(specs, s)
	}
	if opt.resume {
		sess, err := loadSession()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[警告] 无法恢复会话: %v\n", err)
		} else {
			applySessionOptions(sess.Options)
			for _, s := range sess.Tasks {
				addSpec(s)
			}
			fmt.Fprintf(os.Stderr, "[会话] 已恢复 %d 个未完成任务 (%s)\n", len(sess.Tasks), sess.SavedAt.Format("2006-01-02 15:04:05"))
		}
	}
	if opt.inputFile != "" {
		list, err := parseInputFile(opt.inputFile)
		if err != nil {
			fatalf("-i %s: %v", opt.inputFile, err)
		}
		for _, s := range list {
			addSpec(s)
		}
	}
	for _, in := range flag.Args() {
		addSpec(taskSpec{Input: in})
	}
	if len(specs) == 0 && !opt.daemon {
		// 守护模式允许零任务启动:等控制面下指令。
		usage()
		os.Exit(2)
	}
	if opt.stream && len(specs) > 1 {
		fatalf("--stream 一次只能串流一个任务(当前 %d 个)", len(specs))
	}

	// --hash:离线计算 .torrent 的 infohash,不需要网络
	if opt.hashOnly {
		for _, s := range specs {
			mi, err := metainfo.LoadFromFile(s.Input)
			if err != nil {
				fatalf("%s: %v", s.Input, err)
			}
			info, err := mi.UnmarshalInfo()
			if err != nil {
				fatalf("%s: 解析 info 失败: %v", s.Input, err)
			}
			if opt.jsonOutput {
				emitJSON(jsonStatus{Event: "hash", Name: info.Name, InfoHash: mi.HashInfoBytes().HexString(), State: "inspect", Total: info.TotalLength()})
			} else {
				fmt.Fprintf(out, "%s  %s  (%s)\n",
					mi.HashInfoBytes().HexString(), info.Name, human(info.TotalLength()))
			}
		}
		return
	}

	tl := newTaskLimiter()
	cl, err := newClient(tl)
	if err != nil {
		fatalf("初始化客户端失败: %v", err)
	}
	defer cl.Close()

	// Ctrl+C 优雅退出:第一次信号取消 context,让下载/做种循环正常收尾
	// (走 defer cl.Close() 清理连接和端口映射);再按一次才强制退出。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		fmt.Fprintln(out, "\n[提示] 正在停止任务,请稍候(再按一次强制退出)...")
		cancel()
		<-sigCh
		os.Exit(1)
	}()

	m := run(ctx, cl, specs, tl)
	if m == nil {
		return // --all / --dry-run 已完成
	}

	// 守护模式:挂上控制面后常驻运行,等 Ctrl+C 或 RPC shutdown。
	var rpc *rpcServer
	if opt.daemon {
		var err error
		rpc, err = startRPC(ctx, m, opt.rpcListen, opt.rpcSecret, cancel)
		if err != nil {
			fatalf("启动控制面失败: %v", err)
		}
		fmt.Fprintf(out, "[控制面] 已就绪: %s\n", rpc.url)
		fmt.Fprintln(out, "[控制面] JSON-RPC: POST /rpc | REST: /api/tasks | 事件流: GET /events")
		if opt.rpcSecret != "" {
			fmt.Fprintln(out, "[控制面] 已启用令牌鉴权: Authorization: Bearer <token> 或 ?token=<token>")
		}
	}

	// RSS 订阅:常驻轮询订阅源,命中的条目自动入队。
	if opt.rssFile != "" {
		if err := startRSS(ctx, m, opt.rssFile, opt.rssInterval); err != nil {
			fmt.Fprintf(os.Stderr, "[警告] RSS 订阅启动失败: %v\n", err)
		}
	}

	m.Wait()
	if rpc != nil {
		rpc.Close()
	}

	// 边下边播:即使下载结束也保持进程与服务,直到用户 Ctrl+C。
	// 否则播放器刚打开就被断开,失去串流的意义。
	if opt.stream {
		fmt.Fprintln(out, "[流] 下载已完成,服务保持中;按 Ctrl+C 退出")
		<-ctx.Done()
	}
}

// -----------------------------------------------------------------------------
// 参数解析
// -----------------------------------------------------------------------------

func parseFlags() {
	// 配置文件作为 flag 默认值注册,命令行显式参数由 flag.Parse 覆盖。
	cfg := loadConfigFile()
	flag.StringVar(&opt.dir, "dir", cfgStr(cfg, "dir", "."), "下载目录 (同 aria2 -d)")
	flag.StringVar(&opt.dir, "d", opt.dir, "下载目录简写")
	flag.StringVar(&opt.out, "out", cfgStr(cfg, "out", ""), "输出文件名,仅对单文件 torrent 有效 (同 -o)")
	flag.StringVar(&opt.out, "o", opt.out, "输出文件名简写")
	flag.StringVar(&opt.selectFiles, "select", cfgStr(cfg, "select", ""), "选择文件编号,如 1,3-5;0 为全部 (同 aria2 --select-file)")
	flag.StringVar(&opt.selectFiles, "s", opt.selectFiles, "选择文件简写")
	flag.StringVar(&opt.selectFilter, "select-filter", cfgStr(cfg, "select-filter", ""), "按 glob 选文件,如 \"*.mp4\";支持逗号分隔与 ! 排除")
	flag.BoolVar(&opt.all, "all", false, "仅列出 torrent 文件清单后退出")
	flag.BoolVar(&opt.dryRun, "dry-run", false, "仅显示任务信息后退出,不下载")
	flag.BoolVar(&opt.hashOnly, "hash", false, "仅输出 .torrent 的 infohash 后退出")
	flag.BoolVar(&opt.seed, "seed", cfgBool(cfg, "seed", true), "下载完成后继续做种")
	flag.IntVar(&opt.maxConn, "max-conn", cfgInt(cfg, "max-conn", 50), "每个 torrent 的最大对等连接数")
	flag.StringVar(&opt.limitRate, "limit-rate", cfgStr(cfg, "limit-rate", ""), "下载限速,如 512K、5M;每任务独立")
	flag.StringVar(&opt.maxUploadRate, "max-upload-limit", cfgStr(cfg, "max-upload-limit", ""), "上传限速,如 1M;每任务独立")
	flag.BoolVar(&opt.enableDHT, "enable-dht", cfgBool(cfg, "enable-dht", true), "启用 DHT 网络")
	flag.BoolVar(&opt.enablePEX, "enable-pex", cfgBool(cfg, "enable-pex", true), "启用 PEX 对等点交换")
	flag.BoolVar(&opt.enableUTP, "enable-utp", cfgBool(cfg, "enable-utp", true), "启用 uTP 协议")
	flag.BoolVar(&opt.enableHTTP, "enable-http-seed", cfgBool(cfg, "enable-http-seed", true), "启用 HTTP/HTTPS WebSeed 源")
	flag.IntVar(&opt.listenPort, "listen-port", cfgInt(cfg, "listen-port", 0), "BT 监听端口,0 为随机")
	flag.IntVar(&opt.metaTimeout, "meta-timeout", cfgInt(cfg, "meta-timeout", 300), "magnet 获取元数据超时秒数,0 为不限")
	flag.BoolVar(&opt.verify, "verify", cfgBool(cfg, "verify", false), "下载前强制重新校验已有数据(重新哈希分片)")
	flag.IntVar(&opt.stallTimeout, "stall-timeout", cfgInt(cfg, "stall-timeout", 0), "连续 N 秒无下载速度则自动退出,0 禁用")
	flag.Float64Var(&opt.seedRatio, "seed-ratio", cfgFloat(cfg, "seed-ratio", 0), "做种分享率达到该值后停止,如 1.0;0 不限")
	flag.Float64Var(&opt.seedTime, "seed-time", cfgFloat(cfg, "seed-time", 0), "做种 N 分钟后停止;0 不限")
	flag.StringVar(&opt.onComplete, "on-complete", cfgStr(cfg, "on-complete", ""), "任务完成后执行的命令(环境变量传递任务信息)")
	flag.StringVar(&opt.proxy, "proxy", cfgStr(cfg, "proxy", ""), "代理,如 socks5://127.0.0.1:1080 或 http://127.0.0.1:8080")
	flag.BoolVar(&opt.publicTrackers, "public-trackers", cfgBool(cfg, "public-trackers", true), "magnet 自动附加公共 tracker 列表")
	flag.BoolVar(&opt.sequential, "sequential", cfgBool(cfg, "sequential", false), "顺序下载(优先下载靠前的分片,适合边下边播)")
	flag.BoolVar(&opt.disableIPv6, "disable-ipv6", cfgBool(cfg, "disable-ipv6", false), "禁用 IPv6")
	flag.BoolVar(&opt.noPortFwd, "no-port-forwarding", cfgBool(cfg, "no-port-forwarding", false), "禁用 UPnP/NAT-PMP 自动端口映射")
	flag.BoolVar(&opt.noEncrypt, "no-encrypt", cfgBool(cfg, "no-encrypt", false), "禁用 BT 协议加密(等价 --encryption tolerated)")
	flag.StringVar(&opt.encryption, "encryption", cfgStr(cfg, "encryption", "preferred"), "协议加密: required|preferred|tolerated")
	flag.StringVar(&opt.priorityHigh, "priority-high", cfgStr(cfg, "priority-high", ""), "优先下载的文件,编号或 glob,如 1,3 或 \"*.mp4\"")
	flag.Func("tracker", "追加 tracker(可重复或逗号分隔)", func(s string) error {
		for _, u := range strings.Split(s, ",") {
			if u = strings.TrimSpace(u); u != "" {
				opt.trackersCustom = append(opt.trackersCustom, u)
			}
		}
		return nil
	})
	flag.BoolVar(&opt.stream, "stream", false, "边下边播:启动本地 HTTP 流,播放器可直接打开")
	flag.IntVar(&opt.streamPort, "stream-port", cfgInt(cfg, "stream-port", 8080), "流服务端口,0 为随机")
	flag.StringVar(&opt.streamFile, "stream-file", cfgStr(cfg, "stream-file", ""), "指定串流文件(编号或 glob),默认第一个选中文件")
	flag.StringVar(&opt.blocklist, "blocklist", cfgStr(cfg, "blocklist", ""), "IP 黑名单文件,每行 描述:起始IP-结束IP")
	flag.StringVar(&opt.inputFile, "input-file", cfgStr(cfg, "input-file", ""), "批量任务列表文件,每行一个链接")
	flag.StringVar(&opt.inputFile, "i", opt.inputFile, "-i 简写")
	flag.IntVar(&opt.maxConcurrent, "max-concurrent-downloads", cfgInt(cfg, "max-concurrent-downloads", 0), "同时下载的任务数,0 不限")
	flag.BoolVar(&opt.jsonOutput, "json", cfgBool(cfg, "json", false), "每任务一行 JSON 状态输出到 stdout")
	flag.BoolVar(&opt.resume, "resume", false, "恢复 .gotorrent/session.json 中未完成的任务")
	flag.BoolVar(&opt.daemon, "daemon", cfgBool(cfg, "daemon", false), "守护模式:常驻运行并开放控制面")
	flag.StringVar(&opt.rpcListen, "rpc-listen", cfgStr(cfg, "rpc-listen", ""), "控制面监听地址,如 127.0.0.1:6800;0 表示随机端口")
	flag.StringVar(&opt.rpcSecret, "rpc-secret", cfgStr(cfg, "rpc-secret", ""), "控制面令牌鉴权,请求需带 Authorization: Bearer <token>")
	flag.StringVar(&opt.rssFile, "rss-file", cfgStr(cfg, "rss-file", ""), "RSS 订阅配置文件,每行一个订阅")
	flag.IntVar(&opt.rssInterval, "rss-interval", cfgInt(cfg, "rss-interval", 30), "RSS 轮询间隔(分钟),0 关闭")

	flag.Usage = usage
	flag.Parse()

	if opt.stallTimeout < 0 {
		fatalf("--stall-timeout 不能为负数")
	}
	if opt.seedRatio < 0 {
		fatalf("--seed-ratio 不能为负数")
	}
	if opt.seedTime < 0 {
		fatalf("--seed-time 不能为负数")
	}
	if opt.maxConcurrent < 0 {
		fatalf("--max-concurrent-downloads 不能为负数")
	}
	if opt.rssInterval < 0 {
		fatalf("--rss-interval 不能为负数")
	}
	// 守护模式默认监听 127.0.0.1:6800(aria2 同款端口),显式给 0 则随机端口。
	if opt.daemon && opt.rpcListen == "" {
		opt.rpcListen = "127.0.0.1:6800"
	}
	// --no-encrypt 是 --encryption tolerated 的兼容别名,显式设置时覆盖。
	if opt.noEncrypt {
		opt.encryption = "tolerated"
	}
	switch opt.encryption {
	case "required", "preferred", "tolerated":
	default:
		fatalf("--encryption 取值必须是 required / preferred / tolerated")
	}
	if opt.streamPort < 0 || opt.streamPort > 65535 {
		fatalf("--stream-port 必须在 0-65535 之间")
	}
	if opt.stream && opt.out != "" {
		fatalf("--stream 与 -o 重命名不能同时使用(流服务按原文件名读取)")
	}
	// --json 时人读输出全部改走 stderr,保证 stdout 是纯 NDJSON
	if opt.jsonOutput {
		out = os.Stderr
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `gotorrent %s - 基于 anacrolix/torrent 的 BT 下载工具(aria2 风格)

用法: gotorrent [选项] <torrent文件|magnet链接> ...

选项:
  -d, --dir <路径>            下载目录                       [默认: .]
  -o, --out <文件名>          输出文件名(仅单文件 torrent)
  -s, --select <编号>         选择文件,如 "1,3-5"(用 --all 查看编号)
      --select-filter <glob>  按文件名/路径 glob 选择,如 "*.mp4,!*.nfo"
  -i, --input-file <文件>     批量任务列表,每行一个链接,可带行内参数
      --max-concurrent-downloads <n> 同时下载任务数(完成即补位) [默认: 0 不限]
      --resume                恢复上次未完成的任务(~/.gotorrent/session.json)
      --json                  每任务一行 JSON 状态,stdout 为纯 NDJSON
      --all                   仅列出文件清单后退出
      --dry-run               仅显示任务信息,不下载
      --hash                  仅输出 .torrent 的 infohash
      --seed=<bool>           下载完成后是否做种             [默认: true]
      --max-conn <n>          每任务最大连接数               [默认: 50]
      --limit-rate <速度>     下载限速,如 512K、5M
      --max-upload-limit <速度> 上传限速
      --enable-dht=<bool>     启用 DHT                       [默认: true]
      --enable-pex=<bool>     启用 PEX                       [默认: true]
      --enable-utp=<bool>     启用 uTP                       [默认: true]
      --enable-http-seed=<bool> 启用 WebSeed                 [默认: true]
      --listen-port <n>       BT 监听端口,0 随机
      --meta-timeout <秒>     magnet 元数据超时              [默认: 300,0 不限]
      --verify                下载前强制重新校验已有数据
      --stall-timeout <秒>    连续无下载速度 N 秒后自动退出   [默认: 0 禁用]
      --seed-ratio <比值>     做种分享率上限,如 1.0           [默认: 0 不限]
      --seed-time <分钟>      做种时长上限                    [默认: 0 不限]
      --on-complete <命令>    任务完成后执行的命令
      --proxy <地址>          代理,如 socks5://127.0.0.1:1080
      --public-trackers=<bool> magnet 附加公共 tracker        [默认: true]
      --sequential            顺序下载,适合边下边播
      --disable-ipv6          禁用 IPv6
      --no-port-forwarding    禁用 UPnP/NAT-PMP 端口映射
      --encryption <档位>     协议加密 required|preferred|tolerated [默认: preferred]
      --no-encrypt            等同 --encryption tolerated
      --priority-high <规则>  优先下载指定文件,如 "1,3" 或 "*.mp4"
      --tracker <URL>         追加 tracker,可重复或用逗号分隔
      --stream                边下边播,启动本地 HTTP 流
      --stream-port <n>       流服务端口                      [默认: 8080,0 随机]
      --stream-file <规则>    指定串流文件(编号或 glob)
      --blocklist <文件>      IP 黑名单(每行 描述:起始IP-结束IP)

守护模式与控制面:
      --daemon                常驻运行,开放控制面;允许零任务启动
      --rpc-listen <地址>     控制面监听地址            [默认: 127.0.0.1:6800]
      --rpc-secret <令牌>     控制面令牌鉴权(Authorization: Bearer 或 ?token=)
      --rss-file <文件>       RSS 订阅配置文件
      --rss-interval <分钟>   RSS 轮询间隔             [默认: 30]

控制面接口(同一套引擎,三种用法):
  POST /rpc             JSON-RPC 2.0,方法名对齐 aria2:
                         addUri / addTorrent / tellStatus / tellActive /
                         tellWaiting / tellStopped / pause / unpause /
                         remove / getVersion / getGlobalStat / shutdown
  /api/tasks/...        REST:GET 列表、POST 添加、DELETE 移除、
                         POST /api/tasks/{id}/pause 与 /resume
  GET /events            SSE 实时事件流(连接时先推一份全量快照)

子命令:
  create <文件|目录>          制作 .torrent 文件
    -o, --out <路径>          输出文件            [默认: <名称>.torrent]
    -t, --tracker <URL>       tracker,可重复指定多个
    -l, --piece-length <大小> 分片大小,如 256K、1M  [默认: 自动]
    -c, --comment <文本>      备注
    --private                 私有种子(禁用 DHT/PEX)

说明:
  * 断点续传默认开启:下载目录中的 .gotorrent/state.db 记录已校验
    的分片,重启后自动续传(类似 aria2 -c / control 文件)。
  * magnet 获取到的元数据会缓存为 .gotorrent/<infohash>.torrent,
    同一磁力链接下次直接离线加载,无需再联网获取。
  * 支持 DHT/PEX/_tracker/WebSeed 多种来源,磁力链接自动获取元数据。
  * --seed-ratio 与 --seed-time 同时设置时,任一条件达到即停止做种。
  * --on-complete 通过环境变量传递任务信息:GOTRENT_NAME、GOTRENT_HASH、
    GOTRENT_DIR、GOTRENT_PATH、GOTRENT_SIZE;仅对本次运行中新完成的任务触发。
  * --proxy socks5 模式:peer/TCP 与 HTTP 流量全走代理,并禁用 DHT/uTP/
    入站连接(UDP 无法代理)防止泄漏;http 代理仅作用于 HTTP 流量。
  * 配置文件 ~/.gotorrent/config(key=value,# 注释)提供所有选项的默认值,
    命令行显式参数优先;目录、限速等不必每次重复写。
  * 会话文件 ~/.gotorrent/session.json 记录未完成任务及其参数;中断或元数据
    超时后直接 gotorrent --resume 即可继续,下载目录等参数自动还原。
  * -i 输入文件每行格式:<链接|torrent路径> [out=x select=1,3 select-filter=*.mp4
    limit-rate=2M max-upload-limit=512K];任务行内参数只作用于该任务,
    --limit-rate 等全局参数作为缺省;每任务配额独立,总闸按活跃任务求和。
  * --json 时人读提示全部走 stderr,stdout 只输出 NDJSON,event 取值:
    added / start / progress / complete / paused / resumed / removed /
    seeding / error / info / summary / hash / stream。
  * --encryption required 会拒绝未加密的对等点,部分老客户端可能连不上;
    preferred 优先加密但兼容明文(默认);tolerated 不主动要求加密。
  * --priority-high 只改变调度顺序(高优先级分片先请求),不改变"选哪些文件";
    未被 --select/--select-filter 选中的文件仍然跳过。
  * --tracker 在拿到元数据后追加,适合给磁力链接补私有 tracker。
  * --stream 启动后在 stderr 打印 http://127.0.0.1:<port>/stream,播放器直接
    打开即可边下边播(VLC/PotPlayer/mpv);下载完成后服务保持,按 Ctrl+C 退出。
    与 -o 重命名互斥(流服务按原文件名读取)。
  * RSS 订阅配置(--rss-file)按块书写,每块以 url= 开始,后面的键值属于该订阅:
      url=https://example.com/feed.xml
      include=.*1080p.*
      exclude=.*720p.*
      dir=D:/media/movies
    include / exclude 是作用于条目标题的正则,exclude 优先。已处理的条目记在
    <配置文件>.seen,重启后不会重复下载。
  * --daemon 守护模式下任务可随时通过控制面增删改查。暂停的任务连同状态一起
    写进会话,--resume 恢复后仍是暂停状态,需显式 resume 才会继续下载。

示例:
  gotorrent ubuntu.torrent
  gotorrent -d D:\data "magnet:?xt=urn:btih:xxxxxxxx"
  gotorrent --all big.torrent
  gotorrent --select-filter "*.mp4,!*sample*" --seed=false big.torrent
  gotorrent -s 2,4 --limit-rate 2M --seed=false big.torrent
  gotorrent --verify --seed-ratio 1.0 --stall-timeout 120 big.torrent
  gotorrent -i tasks.txt --max-concurrent-downloads 3 --json
  gotorrent --resume
  gotorrent --stream --sequential --seed=false movie.torrent
  gotorrent --encryption required --priority-high "*.mp4" big.torrent
  gotorrent --tracker udp://tracker.opentrackr.org:1337/announce "magnet:?xt=..."
  gotorrent create -t udp://tracker.opentrackr.org:1337/announce ./myfile
`, version)
}

func fatalf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	// --json 模式下致命错误也要在 stdout 留一条结构化记录,便于上游脚本判定失败
	emitError("", msg)
	fmt.Fprintf(os.Stderr, "[错误] %s\n", msg)
	os.Exit(1)
}

// -----------------------------------------------------------------------------
// create 子命令:从文件或目录制作 .torrent
// -----------------------------------------------------------------------------

func runCreate(args []string) {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	var out, pieceLen, comment string
	var private bool
	var trackers []string
	fs.StringVar(&out, "o", "", "输出文件 (默认 <名称>.torrent)")
	fs.StringVar(&out, "out", "", "输出文件")
	fs.Func("t", "tracker URL,可重复", func(s string) error { trackers = append(trackers, s); return nil })
	fs.Func("tracker", "tracker URL,可重复", func(s string) error { trackers = append(trackers, s); return nil })
	fs.StringVar(&pieceLen, "l", "", "分片大小,如 256K、1M (默认自动)")
	fs.StringVar(&pieceLen, "piece-length", "", "分片大小")
	fs.StringVar(&comment, "c", "", "备注")
	fs.StringVar(&comment, "comment", "", "备注")
	fs.BoolVar(&private, "private", false, "私有种子(禁用 DHT/PEX)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `用法: gotorrent create [选项] <文件|目录>

选项:
  -o, --out <路径>          输出文件            [默认: <名称>.torrent]
  -t, --tracker <URL>       tracker,可重复指定多个
  -l, --piece-length <大小> 分片大小,如 256K、1M  [默认: 自动]
  -c, --comment <文本>      备注
      --private             私有种子(禁用 DHT/PEX)

示例:
  gotorrent create ./myfile.iso
  gotorrent create -t udp://tracker.opentrackr.org:1337/announce -l 1M ./mydir
`)
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	root, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		fatalf("%v", err)
	}
	if _, err := os.Stat(root); err != nil {
		fatalf("无法访问 %s: %v", root, err)
	}

	var info metainfo.Info
	if pieceLen != "" {
		n, err := parseSize(pieceLen)
		if err != nil || n <= 0 {
			fatalf("--piece-length: 非法分片大小 %q", pieceLen)
		}
		if n&(n-1) != 0 {
			fatalf("--piece-length: 分片大小必须是 2 的幂")
		}
		info.PieceLength = n
	}
	fmt.Printf("[制作] 正在哈希 %s ...\n", root)
	if err := info.BuildFromFilePath(root); err != nil {
		fatalf("构建种子信息失败: %v", err)
	}
	if info.TotalLength() == 0 || info.NumPieces() == 0 {
		fatalf("没有可制作的内容(空目录或空文件)")
	}
	if private {
		info.Private = &private
	}
	infoBytes, err := bencode.Marshal(&info)
	if err != nil {
		fatalf("编码 info 失败: %v", err)
	}
	mi := metainfo.MetaInfo{
		InfoBytes:    infoBytes,
		Comment:      comment,
		CreatedBy:    "gotorrent " + version,
		CreationDate: time.Now().Unix(),
	}
	if len(trackers) > 0 {
		mi.Announce = trackers[0]
		mi.AnnounceList = [][]string{trackers}
	}
	if out == "" {
		out = info.Name + ".torrent"
	}
	if _, err := os.Lstat(out); err == nil {
		fatalf("输出文件已存在,拒绝覆盖: %s", out)
	}
	f, err := os.Create(out)
	if err != nil {
		fatalf("创建输出文件失败: %v", err)
	}
	if err := mi.Write(f); err != nil {
		f.Close()
		os.Remove(out)
		fatalf("写入 torrent 失败: %v", err)
	}
	if err := f.Close(); err != nil {
		fatalf("关闭输出文件失败: %v", err)
	}
	fmt.Printf("[完成] %s\n", out)
	fmt.Printf("  名称 %s | 大小 %s | 分片 %d × %s\n",
		info.Name, human(info.TotalLength()), info.NumPieces(), human(info.PieceLength))
	fmt.Printf("  哈希 %s\n", mi.HashInfoBytes().HexString())
}

// -----------------------------------------------------------------------------
// 客户端初始化
// -----------------------------------------------------------------------------

func newClient(tl *taskLimiter) (*torrent.Client, error) {
	// anacrolix/log 在包 init() 阶段读取 GO_LOG,进程内再设置环境变量已无效,
	// 这里直接把全局 logger 的 handler 换成丢弃,静默库内部日志。
	log.Default.SetHandlers(log.DiscardHandler)

	cfg := torrent.NewDefaultClientConfig()
	cfg.Seed = opt.seed
	cfg.NoUpload = !opt.seed
	cfg.NoDHT = !opt.enableDHT
	cfg.DisablePEX = !opt.enablePEX
	cfg.DisableUTP = !opt.enableUTP
	cfg.DisableWebseeds = !opt.enableHTTP
	cfg.EstablishedConnsPerTorrent = opt.maxConn
	cfg.ListenPort = opt.listenPort
	cfg.DisableIPv6 = opt.disableIPv6
	cfg.NoDefaultPortForwarding = opt.noPortFwd
	// 加密三档:
	//   required   -> 强制头部混淆(未加密连接会被拒)
	//   preferred  -> 优先加密,也接受明文(默认)
	//   tolerated  -> 不主动要求加密(= 旧 --no-encrypt 行为)
	switch opt.encryption {
	case "required":
		cfg.HeaderObfuscationPolicy.Preferred = true
		cfg.HeaderObfuscationPolicy.RequirePreferred = true
	case "preferred":
		cfg.HeaderObfuscationPolicy.Preferred = true
		cfg.HeaderObfuscationPolicy.RequirePreferred = false
	case "tolerated":
		cfg.HeaderObfuscationPolicy.Preferred = false
		cfg.HeaderObfuscationPolicy.RequirePreferred = false
	}
	if opt.blocklist != "" {
		bl, err := loadBlocklist(opt.blocklist)
		if err != nil {
			return nil, fmt.Errorf("--blocklist: %w", err)
		}
		cfg.IPBlocklist = bl
	}
	var peerDialer torrent.Dialer
	if opt.proxy != "" {
		d, err := applyProxy(cfg, opt.proxy)
		if err != nil {
			return nil, fmt.Errorf("--proxy: %w", err)
		}
		peerDialer = d
	}

	baseDir, err := absDir(opt.dir)
	if err != nil {
		return nil, err
	}
	// 文件存储 + 分片完成记录:实现断点续传。
	// 注意:sqlite 版 NewSqlitePieceCompletion 需要 cgo,纯 Go 交叉编译时
	// 不可用,这里用等效的 bbolt 实现。
	stateDir, err := absDir(filepath.Join(baseDir, ".gotorrent"))
	if err != nil {
		return nil, err
	}
	completion, err := storage.NewBoltPieceCompletion(stateDir)
	if err != nil {
		// bbolt 对同一文件是独占的:同一下载目录再起一个实例必然拿不到锁。
		// 这个报错信息对用户来说太含糊,补一句提示。
		return nil, fmt.Errorf("打开断点数据库失败: %w(该下载目录可能已被另一个 gotorrent 实例占用)", err)
	}
	cfg.DefaultStorage = storage.NewFileWithCompletion(baseDir, completion)

	// 限速总闸由 taskLimiter 按活跃任务的独立配额动态调整。
	// 注意不能传 nil 或 limit=0:checkConfig 会把 0 判为错误配置。
	cfg.DownloadRateLimiter = tl.downloadLimiter()
	cfg.UploadRateLimiter = tl.uploadLimiter()

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if peerDialer != nil {
		cl.AddDialer(peerDialer)
	}
	return cl, nil
}

// applyProxy 配置代理并返回 peer 拨号器(无需代理拨号时返回 nil)。
// socks5:DialFirst 会让所有拨号器竞态,必须移除默认直连拨号器
// (DialForPeerConns=false),否则代理会被直连绕过造成泄漏;
// UDP(DHT/uTP)无法经 socks5 代理,一并禁用,入站连接也关闭。
// http:x/net/proxy 不支持 http 方案,只把 HTTP 流量(tracker/WebSeed)走代理,
// peer TCP 仍直连,明确提示用户。
func applyProxy(cfg *torrent.ClientConfig, raw string) (torrent.Dialer, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("非法代理地址 %q", raw)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		d, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, err
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("该代理类型不支持 context 拨号")
		}
		cfg.DialForPeerConns = false
		cfg.AcceptPeerConnections = false
		cfg.NoDHT = true
		cfg.DisableUTP = true
		cfg.TrackerDialContext = cd.DialContext
		cfg.HTTPDialContext = cd.DialContext
		fmt.Fprintln(os.Stderr, "[代理] socks5 模式:已禁用 DHT/uTP/入站(UDP 无法代理),TCP 全走代理")
		return socksDialer{cd}, nil
	case "http", "https":
		cfg.HTTPProxy = http.ProxyURL(u)
		fmt.Fprintln(os.Stderr, "[代理] http 模式:仅 HTTP 流量(tracker/WebSeed)走代理,peer 连接仍直连")
		return nil, nil
	default:
		return nil, fmt.Errorf("不支持的代理方案 %q(支持 socks5/http)", u.Scheme)
	}
}

type socksDialer struct{ d proxy.ContextDialer }

func (s socksDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return s.d.DialContext(ctx, "tcp", addr)
}

func (s socksDialer) DialerNetwork() string { return "tcp" }

// loadBlocklist 加载 P2P 格式 IP 黑名单(每行 描述:起始IP-结束IP)
func loadBlocklist(path string) (*iplist.IPList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bl, err := iplist.NewFromReader(f)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "[黑名单] 已加载 %d 条 IP 段\n", bl.NumRanges())
	return bl, nil
}

// -----------------------------------------------------------------------------
// 添加任务(.torrent 文件或 magnet 链接)
// -----------------------------------------------------------------------------

func isMagnet(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "magnet:")
}

func describeInput(in string, t *torrent.Torrent) string {
	if isMagnet(in) {
		return "magnet " + t.InfoHash().HexString()
	}
	return filepath.Base(in) + " " + t.InfoHash().HexString()
}

// addInput 添加任务,并统一追加 --tracker 指定的 tracker。
// 必须在拿到元数据之前追加:magnet 的元数据本身就要靠 tracker 获取,
// 放到 activate(已拿到 Info)里就晚了。
func addInput(cl *torrent.Client, in string) (*torrent.Torrent, error) {
	t, err := addInputRaw(cl, in)
	if err == nil && t != nil && len(opt.trackersCustom) > 0 {
		t.AddTrackers([][]string{opt.trackersCustom})
	}
	return t, err
}

func addInputRaw(cl *torrent.Client, in string) (*torrent.Torrent, error) {
	if isMagnet(in) {
		in = strings.TrimSpace(in)
		if t := addFromMetaCache(cl, in); t != nil {
			return t, nil
		}
		return cl.AddMagnet(in)
	}
	mi, err := metainfo.LoadFromFile(in)
	if err != nil {
		return nil, err
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, fmt.Errorf("解析 torrent 元数据失败: %w", err)
	}
	// UrlList(webseed) 字段在 spec 转换时可能为空,这里兜底补上
	if len(spec.Webseeds) == 0 && len(mi.UrlList) > 0 {
		spec.Webseeds = mi.UrlList
	}
	t, _, err := cl.AddTorrentSpec(spec)
	return t, err
}

// metaCachePath 返回 magnet 元数据缓存文件路径:.gotorrent/<infohash>.torrent
func metaCachePath(infoHash metainfo.Hash) string {
	return filepath.Join(mustAbs(opt.dir), ".gotorrent", infoHash.HexString()+".torrent")
}

// addFromMetaCache 尝试用本地缓存的元数据离线加载 magnet,失败返回 nil 回退联网获取。
func addFromMetaCache(cl *torrent.Client, uri string) *torrent.Torrent {
	m, err := metainfo.ParseMagnetUri(uri)
	if err != nil {
		return nil
	}
	mi, err := metainfo.LoadFromFile(metaCachePath(m.InfoHash))
	if err != nil {
		return nil
	}
	if mi.HashInfoBytes() != m.InfoHash {
		return nil
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil
	}
	t, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		return nil
	}
	// 缓存里的 tracker 是上次获取时的,合并本次磁力链接自带的 tr= 参数
	if len(m.Trackers) > 0 {
		t.AddTrackers([][]string{m.Trackers})
	}
	fmt.Fprintf(out, "[缓存] %s 命中本地元数据缓存,跳过联网获取\n", m.InfoHash.HexString()[:16])
	return t
}

// saveMetaCache 把 magnet 获取到的元数据落盘,同一磁力链接下次可离线加载。
// 先写临时文件再改名,避免留下半截缓存。
func saveMetaCache(t *torrent.Torrent) {
	path := metaCachePath(t.InfoHash())
	if _, err := os.Stat(path); err == nil {
		return
	}
	mi := t.Metainfo()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".meta-*.tmp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[警告] 保存元数据缓存失败: %v\n", err)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := mi.Write(tmp); err != nil {
		tmp.Close()
		fmt.Fprintf(os.Stderr, "[警告] 保存元数据缓存失败: %v\n", err)
		return
	}
	if err := tmp.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "[警告] 保存元数据缓存失败: %v\n", err)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		fmt.Fprintf(os.Stderr, "[警告] 保存元数据缓存失败: %v\n", err)
		return
	}
	fmt.Fprintf(out, "[缓存] 元数据已保存: %s\n", filepath.Base(path))
}

// -----------------------------------------------------------------------------
// 下载主流程
// -----------------------------------------------------------------------------

// task 跟踪单个 torrent 的下载状态
type task struct {
	id         string // 任务 ID = infohash 十六进制,跨重启稳定
	t          *torrent.Torrent
	name       string
	spec       taskSpec // 该任务的参数(独立限速、选择规则、会话恢复)
	state      taskState
	created    time.Time
	err        string       // 失败原因(state == stateError 时有效)
	selected   map[int]bool // 被选中的文件编号(1-based),与实时优先级解耦
	selFiles   []*torrent.File
	highSet    map[int]bool // --priority-high 命中的文件编号
	wanted     int64        // 需要下载的文件总字节数
	wantPieces []int        // 被选中文件覆盖的分片下标(完成判定基准)
	seedStart  time.Time    // 进入做种状态的时间(做种时长上限从此刻起算)
	prevRead   int64        // 上轮统计:有效下载字节
	prevWrite  int64        // 上轮统计:上传字节
	lastSample time.Time
	speed      float64
	upSpeed    float64
	done       bool
	bar        *mpb.Bar // mpb 进度条
	barDone    bool     // 进度条是否已标记完成
	renameOut  string   // -o 目标文件名(仅单文件 torrent)
	renamed    bool     // 是否已执行过重命名
	renamedTo  string   // 重命名成功后的相对路径(供钩子取真实路径)
}

// wantedBytesDone 统计被选中文件的已完成字节数。
//
// 注意:不能直接按"当前优先级 != None"过滤——暂停时所有文件优先级被降到
// None,那样算出来永远是 0,进度条会倒退。File.BytesCompleted() 本身与
// 优先级无关(是已完成分片之和),所以按缓存的选中集合统计才正确。
func (tk *task) wantedBytesDone() int64 {
	var n int64
	for _, f := range tk.selFiles {
		n += f.BytesCompleted()
	}
	return n
}

// snapshot 返回任务的状态快照,供 --json 输出与 RPC 控制面共用。
func (tk *task) snapshot() jsonStatus {
	var st torrent.TorrentStats
	if tk.t != nil {
		st = tk.t.Stats()
	}
	done := tk.wantedBytesDone()
	pct := 0.0
	if tk.wanted > 0 {
		pct = float64(done) / float64(tk.wanted) * 100
	}
	eta := 0.0
	if tk.speed > 0 && tk.wanted > done {
		eta = float64(tk.wanted-done) / tk.speed
	}
	return jsonStatus{
		ID:            tk.id,
		Name:          tk.name,
		InfoHash:      tk.infoHash(),
		Input:         tk.spec.Input,
		State:         string(tk.state),
		Total:         tk.wanted,
		Completed:     done,
		Progress:      round2(pct),
		DownloadSpeed: round2(tk.speed),
		UploadSpeed:   round2(tk.upSpeed),
		Peers:         st.ActivePeers,
		Seeders:       st.ConnectedSeeders,
		ETA:           round2(eta),
		Active:        0,
		Queued:        0,
		Error:         tk.err,
		CreatedAt:     tk.created.Format(time.RFC3339),
	}
}

// infoHash 返回任务的 infohash;尚未加入客户端时回退到 ID。
func (tk *task) infoHash() string {
	if tk.t != nil {
		return tk.t.InfoHash().HexString()
	}
	return tk.id
}

// complete 判断所有"需要下载"的分片是否已完成。
// 注意:不能用 t.Complete() —— 它要求全部分片完成,部分选择文件时
// 未选中分片永不下载,它永远不会为 true(任务会卡在 100% 永不退出);
// 也不能用 PieceState.Priority —— 那是 effectivePriority(),
// 会把"已完成"和"完成状态未知"的分片都报成 None。
func (tk *task) complete() bool {
	for _, i := range tk.wantPieces {
		if !tk.t.PieceState(i).Complete {
			return false
		}
	}
	return true
}

// publicTrackers 是常用公共 tracker,magnet 下载时附加以提高找到 peer 的概率
var publicTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.tracker.cl:1337/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.tiny-vps.com:6969/announce",
	"udp://explodie.org:6969/announce",
	"https://tracker.tamersunion.org:443/announce",
}

// sequentialWindow 把最靠前的未完成分片及其后 window 个分片提升为最高优先级,
// 随下载推进滑动,实现近似顺序下载(边下边播)。之前窗口里没下完的分片
// 保持高优先级继续优先重试,符合顺序播放的诉求。
func sequentialWindow(ctx context.Context, t *torrent.Torrent, wantPieces []int) {
	const window = 16
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		first := -1
		for i, idx := range wantPieces {
			if !t.PieceState(idx).Complete {
				first = i
				break
			}
		}
		if first < 0 {
			return
		}
		end := first + window
		if end > len(wantPieces) {
			end = len(wantPieces)
		}
		for _, idx := range wantPieces[first:end] {
			t.Piece(idx).SetPriority(torrent.PiecePriorityNow)
		}
	}
}

func refreshTasks(tasks []*task, now time.Time) (justDone []*task) {
	for _, tk := range tasks {
		st := tk.t.Stats()
		read := st.BytesReadUsefulData.Int64()
		write := st.BytesWrittenData.Int64()
		dt := now.Sub(tk.lastSample).Seconds()
		if dt > 0 {
			tk.speed = delta(read, tk.prevRead) / dt
			tk.upSpeed = delta(write, tk.prevWrite) / dt
		}
		tk.prevRead, tk.prevWrite, tk.lastSample = read, write, now
		if !tk.done && tk.complete() {
			tk.done = true
			justDone = append(justDone, tk)
		}
	}
	return
}

func delta(cur, prev int64) float64 {
	if cur < prev {
		return 0
	}
	return float64(cur - prev)
}

func countActive(tasks []*task) int {
	n := 0
	for _, tk := range tasks {
		if !tk.done {
			n++
		}
	}
	return n
}

func totalWanted(tasks []*task) int64 {
	var n int64
	for _, tk := range tasks {
		n += tk.wanted
	}
	return n
}

func anySeeder(tasks []*task) bool {
	for _, tk := range tasks {
		if tk.done {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// 进度展示
// -----------------------------------------------------------------------------

func printTaskHeader(t *torrent.Torrent) {
	info := t.Info()
	fmt.Fprintf(out, "\n名称: %s\n", info.Name)
	fmt.Fprintf(out, "  大小 %s | 分片 %d × %s | 哈希 %s\n",
		human(info.TotalLength()), t.NumPieces(), human(info.PieceLength), t.InfoHash().HexString())
}

// printFileList 打印文件清单;selSpec / filterSpec 为该任务自身的选择规则
// (来自 -i 行内参数或 --resume),为空时回退到全局 opt。
func printFileList(t *torrent.Torrent, selSpec, filterSpec string) {
	files := t.Files()
	if len(files) < 2 {
		return
	}
	set := selectedSet(t, firstNonEmpty(selSpec, opt.selectFiles), firstNonEmpty(filterSpec, opt.selectFilter))
	fmt.Fprintf(out, "  文件列表 (* 为选中;--select 选编号 / --select-filter 选 glob):\n")
	for i, f := range files {
		mark := " "
		if set[i+1] {
			mark = "*"
		}
		fmt.Fprintf(out, "   %s %3d. %12s  %s\n", mark, i+1, human(f.Length()), f.DisplayPath())
	}
}

func printSummary(tasks []*task, dur time.Duration) {
	if dur <= 0 {
		dur = time.Second
	}
	fmt.Fprintf(out, "%-40s %14s %14s %12s\n", "任务", "下载", "上传", "均速")
	var td, tu int64
	for _, tk := range tasks {
		st := tk.t.Stats()
		d := st.BytesReadUsefulData.Int64()
		u := st.BytesWrittenData.Int64()
		td += d
		tu += u
		fmt.Fprintf(out, "%-40s %14s %14s %12s\n", trunc(tk.name, 38), human(d), human(u), humanBps(int64(float64(d)/dur.Seconds())))
	}
	fmt.Fprintf(out, "%-40s %14s %14s %12s\n", "总计", human(td), human(tu), humanBps(int64(float64(td)/dur.Seconds())))
}

// applyRename 在任务完成时同步执行 -o 重命名。
// 旧实现用 goroutine 等 Complete().On(),--seed=false 时进程可能在改名执行前
// 就退出;移入主循环后由 done 判定直接触发,退出前必定执行。
// w 传 *mpb.Progress 可让消息安全地显示在进度条上方。
func (tk *task) applyRename(w io.Writer) {
	if tk.renameOut == "" || tk.renamed {
		return
	}
	tk.renamed = true
	files := tk.t.Files()
	if len(files) != 1 {
		return
	}
	src := filepath.Join(mustAbs(opt.dir), filepath.FromSlash(files[0].DisplayPath()))
	dst := filepath.Join(mustAbs(opt.dir), filepath.FromSlash(tk.renameOut))
	if src == dst {
		return
	}
	if _, err := os.Lstat(dst); err == nil {
		fmt.Fprintf(w, "[警告] 目标已存在,跳过重命名: %s\n", dst)
		return
	}
	if err := os.Rename(src, dst); err != nil {
		fmt.Fprintf(w, "[警告] 重命名失败: %v\n", err)
		return
	}
	tk.renamedTo = tk.renameOut
	fmt.Fprintf(w, "[重命名] %s -> %s\n", filepath.Base(src), tk.renameOut)
}

// runOnComplete 在任务新完成时同步执行 --on-complete 命令,通过环境变量
// 传递任务信息(名称/哈希/目录/路径/字节数)。失败只警告,不影响做种。
// 同步执行是刻意的:aria2 风格钩子应在进程退出前跑完;Ctrl+C 取消 ctx
// 会连带杀掉钩子进程,不会卡住退出。
func (tk *task) runOnComplete(ctx context.Context, w io.Writer) {
	if opt.onComplete == "" {
		return
	}
	dir := mustAbs(opt.dir)
	path := filepath.Join(dir, filepath.FromSlash(tk.t.Info().Name))
	if files := tk.t.Files(); len(files) == 1 {
		rel := files[0].DisplayPath()
		if tk.renamedTo != "" {
			rel = tk.renamedTo
		}
		path = filepath.Join(dir, filepath.FromSlash(rel))
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", opt.onComplete)
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", opt.onComplete)
	}
	cmd.Env = append(os.Environ(),
		"GOTRENT_NAME="+tk.name,
		"GOTRENT_HASH="+tk.t.InfoHash().HexString(),
		"GOTRENT_DIR="+dir,
		"GOTRENT_PATH="+path,
		"GOTRENT_SIZE="+strconv.FormatInt(tk.wanted, 10),
	)
	fmt.Fprintf(w, "[钩子] %s 完成,执行: %s\n", trunc(tk.name, 40), opt.onComplete)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		fmt.Fprintf(w, "%s", out)
		if out[len(out)-1] != '\n' {
			fmt.Fprintln(w)
		}
	}
	if err != nil {
		fmt.Fprintf(w, "[警告] --on-complete 执行失败: %v\n", err)
	}
}

// -----------------------------------------------------------------------------
// 做种
// -----------------------------------------------------------------------------

// 做种的时长 / 分享率上限由引擎在主循环里逐任务判定(见 engine.go),
// 这里只保留上限的提示文案。
func printSeedLimits() {
	switch {
	case opt.seedRatio > 0 && opt.seedTime > 0:
		fmt.Fprintf(out, "[做种上限] 分享率 %.2f 或 %g 分钟,任一达到即停止\n", opt.seedRatio, opt.seedTime)
	case opt.seedRatio > 0:
		fmt.Fprintf(out, "[做种上限] 分享率达到 %.2f 即停止\n", opt.seedRatio)
	case opt.seedTime > 0:
		fmt.Fprintf(out, "[做种上限] %g 分钟后停止\n", opt.seedTime)
	}
}

// -----------------------------------------------------------------------------
// 文件选择
// -----------------------------------------------------------------------------

type selection struct {
	all bool
	set map[int]bool
}

// parseSelect 解析 "1,3-5" 之类的编号表达式;"0" 或空表示全部
func parseSelect(spec string) selection {
	sel := selection{all: true, set: map[int]bool{}}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return sel
	}
	sel.all = false
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "0" {
			return selection{all: true, set: map[int]bool{}}
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err1 := strconv.Atoi(strings.TrimSpace(lo))
			b, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 == nil && err2 == nil {
				for i := a; i <= b; i++ {
					sel.set[i] = true
				}
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			sel.set[n] = true
		}
	}
	return sel
}

// wantedBytes 统计被选中文件的总字节数
func wantedBytes(t *torrent.Torrent) int64 {
	var n int64
	for _, f := range t.Files() {
		if f.Priority() != torrent.PiecePriorityNone {
			n += f.Length()
		}
	}
	return n
}

// downloadAllFiles 标记全部文件为下载。
// 注意:不能改用 t.DownloadAll() —— 它只提升分片(piece)优先级,
// 不会设置文件(file)级优先级,而 wantedBytes() 依赖文件级优先级
// (PiecePriorityNone 是零值),会导致 wanted=0、任务被误判为"已存在"而跳过下载。
func downloadAllFiles(files []*torrent.File) {
	for _, f := range files {
		f.Download()
	}
}

// wantedPieceIndexes 返回被选中文件覆盖的分片下标(升序去重)。
// 注意:不能改用 PieceState.Priority 判断"是否需要下载" —— 那是
// effectivePriority(),会把"已完成"和"完成状态未知"的分片都报成 None。
//
// 引擎用的是 pieceIndexesOfFiles(按缓存的选中文件集合计算),因为暂停时
// 文件优先级会被降到 None,再按优先级过滤就分不清"没选"和"已暂停"了。
func wantedPieceIndexes(t *torrent.Torrent) []int {
	return pieceIndexesOfFiles(t.Files())
}

// -----------------------------------------------------------------------------
// 通用辅助
// -----------------------------------------------------------------------------

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	mul := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mul, s = 1<<10, s[:len(s)-1]
	case 'm', 'M':
		mul, s = 1<<20, s[:len(s)-1]
	case 'g', 'G':
		mul, s = 1<<30, s[:len(s)-1]
	case 'b', 'B':
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("非法速度值 %q", s)
	}
	return int64(f * float64(mul)), nil
}

func absDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	return abs, nil
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanBps(n int64) string {
	return human(n) + "/s"
}

func etaStr(seconds float64) string {
	if seconds > 36*3600 {
		return "很久"
	}
	d := time.Duration(seconds) * time.Second
	if d >= time.Hour {
		return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

func trunc(s string, n int) string {
	r := []rune(s)
	// 名称来自不可信的 torrent 元数据,过滤控制字符防止 ANSI 转义扰乱终端
	for i, c := range r {
		if c < 0x20 || c == 0x7f {
			r[i] = ' '
		}
	}
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
