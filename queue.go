package main

// 任务检视与进度条装配。
//
// 下载主循环已经搬进 engine.go 的 Manager(CLI 与 RPC 守护模式共用同一套
// 调度逻辑),这里保留的是"不下载、只看"的两条路径(--all / --dry-run),
// 以及 mpb 进度条的装配。

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

// run 是下载主流程:交给引擎统一调度。
//
// CLI 与 RPC 守护模式共用同一个 Manager,差别只在 daemon 标志:
// 守护模式主循环不会自己退出,一直等外部命令(Ctrl+C / RPC shutdown)。
// 返回引擎实例,供调用方挂控制面;--all / --dry-run 时返回 nil。
func run(ctx context.Context, cl *torrent.Client, specs []taskSpec, tl *taskLimiter) *Manager {
	if opt.all || opt.dryRun {
		runInspect(ctx, cl, specs)
		return nil
	}
	m := newManager(ctx, cl, tl)
	m.daemon = opt.daemon
	m.Start()
	// CLI 侧的事件消费者:--json 模式下把引擎事件转成 NDJSON 输出。
	// 人读模式下引擎自己已经打印了提示,这里不再重复输出。
	events, unsub := m.Subscribe()
	go func() {
		defer unsub()
		for ev := range events {
			if opt.jsonOutput {
				s := ev.task
				s.Event = string(ev.kind)
				emitJSON(s)
			}
		}
	}()
	m.AddAll(specs)
	return m
}

// runInspect 处理 --all / --dry-run:不下载、不写会话。
func runInspect(ctx context.Context, cl *torrent.Client, specs []taskSpec) {
	type entry struct {
		spec taskSpec
		t    *torrent.Torrent
	}
	var entries []entry
	for _, s := range specs {
		t, err := addInput(cl, s.Input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[错误] 添加任务失败 %s: %v\n", s.Input, err)
			emitError(s.Input, err.Error())
			continue
		}
		entries = append(entries, entry{s, t})
	}
	if len(entries) == 0 {
		return
	}
	metaCtx := ctx
	if opt.metaTimeout > 0 {
		var cancel context.CancelFunc
		metaCtx, cancel = context.WithTimeout(ctx, time.Duration(opt.metaTimeout)*time.Second)
		defer cancel()
	}
	ready := make(chan *torrent.Torrent, len(entries))
	for _, e := range entries {
		go func(t *torrent.Torrent) {
			select {
			case <-t.GotInfo():
				ready <- t
			case <-metaCtx.Done():
			}
		}(e.t)
	}
	got := map[*torrent.Torrent]bool{}
	for i := 0; i < len(entries); i++ {
		select {
		case t := <-ready:
			got[t] = true
		case <-metaCtx.Done():
			i = len(entries)
		}
	}
	for _, e := range entries {
		if !got[e.t] {
			fmt.Fprintf(os.Stderr, "[警告] %s 获取元数据超时\n", e.t.InfoHash().HexString()[:16])
			emitError(e.spec.Input, "metadata timeout")
			continue
		}
		if opt.jsonOutput {
			emitTorrentInfo(e.t, e.spec)
		} else {
			printTaskHeader(e.t)
			printFileList(e.t, firstNonEmpty(e.spec.Select, opt.selectFiles), firstNonEmpty(e.spec.SelectFilter, opt.selectFilter))
		}
	}
	if opt.dryRun && !opt.jsonOutput {
		fmt.Fprintln(out, "\n[dry-run] 未开始下载")
	}
}

func emitTorrentInfo(t *torrent.Torrent, s taskSpec) {
	files := t.Files()
	set := selectedSet(t, firstNonEmpty(s.Select, opt.selectFiles), firstNonEmpty(s.SelectFilter, opt.selectFilter))
	list := make([]jsonFile, 0, len(files))
	for i, f := range files {
		list = append(list, jsonFile{
			Index: i + 1, Path: f.DisplayPath(), Size: f.Length(), Selected: set[i+1],
		})
	}
	emitJSON(jsonStatus{
		Event: "info", Name: t.Info().Name, InfoHash: t.InfoHash().HexString(),
		Input: s.Input, State: "inspect", Total: t.Info().TotalLength(), Files: list,
	})
}

func attachBar(p *mpb.Progress, tk *task) {
	cur := tk
	total := cur.wanted
	if total <= 0 {
		total = 1
	}
	var prevRead, prevWrite int64
	var prevSample time.Time
	cur.bar = p.AddBar(total,
		mpb.PrependDecorators(decor.Name(trunc(cur.name, 26), decor.WCSyncSpace)),
		mpb.AppendDecorators(
			decor.CountersKibiByte("% .1f / % .1f", decor.WCSyncSpace),
			decor.Any(func(s decor.Statistics) string {
				st := cur.t.Stats()
				now := time.Now()
				var down, up float64
				if dt := now.Sub(prevSample).Seconds(); dt > 0 && !prevSample.IsZero() {
					down = delta(st.BytesReadUsefulData.Int64(), prevRead) / dt
					up = delta(st.BytesWrittenData.Int64(), prevWrite) / dt
				}
				prevRead, prevWrite = st.BytesReadUsefulData.Int64(), st.BytesWrittenData.Int64()
				prevSample = now
				eta := "--:--"
				if s.Current >= cur.wanted {
					eta = "00:00"
				} else if down > 0 {
					eta = etaStr(float64(cur.wanted-s.Current) / down)
				}
				return fmt.Sprintf(" ↓%s ↑%s 对%d/种%d ETA %s",
					humanBps(int64(down)), humanBps(int64(up)), st.ActivePeers, st.ConnectedSeeders, eta)
			}),
		),
		mpb.BarRemoveOnComplete(),
	)
}
