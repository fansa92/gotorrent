package main

// 下载队列调度:--max-concurrent-downloads 限制同时激活的任务数,
// 任务完成即补位(不是分批),元数据获取异步进行,不阻塞调度循环。

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

type pendingJob struct {
	spec     taskSpec
	t        *torrent.Torrent
	isMagnet bool
}

func concurrentLimit(total int) int {
	if opt.maxConcurrent <= 0 || opt.maxConcurrent > total {
		return total
	}
	return opt.maxConcurrent
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

// run 是下载主流程:队列调度 + 进度循环 + 汇总 + 做种。
func run(ctx context.Context, cl *torrent.Client, specs []taskSpec, tl *taskLimiter) {
	if opt.all || opt.dryRun {
		runInspect(ctx, cl, specs)
		return
	}

	pending := append([]taskSpec(nil), specs...)
	var failed []taskSpec                          // 元数据获取失败/被中断,保留在会话中但不重试
	var tasks []*task                              // 所有已激活的任务(含已完成,供汇总/做种)
	starting := map[*torrent.Torrent]*pendingJob{} // 已加入、等待元数据
	readyCh := make(chan *pendingJob, 16)
	failCh := make(chan *pendingJob, 16)

	var p *mpb.Progress
	if !opt.jsonOutput {
		// 刷新频率与数据采样(1s)对齐。WithAutoRefresh 是必须的:
		// mpb 在输出不是终端(重定向/管道)时会进入 noRenderMode,既不画进度条,
		// 也会把通过 p.Write() 输出的重命名/钩子消息永远留在缓冲区里丢失。
		// 开启后:终端里仍是原地刷新;非终端则每个周期追加一行纯文本进度,
		// 日志可读且消息不会丢。
		// 宽度给足:装饰项(名称/计数/速度/ETA)合计约 60 列,太窄会把进度条本体挤成 0 宽。
		// 真实终端下 mpb 会改用实际终端宽度,这里只影响非终端日志。
		p = mpb.New(mpb.WithWidth(100), mpb.WithRefreshRate(time.Second), mpb.WithAutoRefresh(), mpb.WithOutput(out))
	}

	// writeSession 记录"仍未完成"的任务:排队中 + 等待元数据 + 下载中未完成。
	writeSession := func() {
		remain := append([]taskSpec(nil), pending...)
		remain = append(remain, failed...)
		for _, j := range starting {
			remain = append(remain, j.spec)
		}
		for _, tk := range tasks {
			if !tk.done {
				remain = append(remain, tk.spec)
			}
		}
		sessionWarn(saveSession(remain))
	}
	writeSession()

	msgWriter := func() io.Writer {
		if p != nil {
			return p
		}
		return out
	}

	start := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	stallSecs := 0
	interrupted, stalled := false, false
	limit := concurrentLimit(len(specs))

	// fill 把排队任务补进空位。
	fill := func() {
		for len(pending) > 0 && len(starting)+countActive(tasks) < limit {
			s := pending[0]
			pending = pending[1:]
			t, err := addInput(cl, s.Input)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[错误] 添加任务失败 %s: %v\n", s.Input, err)
				emitError(s.Input, err.Error())
				continue
			}
			j := &pendingJob{spec: s, t: t, isMagnet: isMagnet(s.Input)}
			starting[t] = j
			if !opt.jsonOutput {
				fmt.Fprintf(out, "[任务] 已添加: %s\n", describeInput(s.Input, t))
			}
			go func(j *pendingJob) {
				metaCtx, cancel := metaContext(ctx)
				defer cancel()
				select {
				case <-j.t.GotInfo():
					readyCh <- j
				case <-metaCtx.Done():
					failCh <- j
				}
			}(j)
		}
	}

	// activate 元数据就绪:应用选择、校验、建 task/进度条、启动顺序下载。
	activate := func(j *pendingJob) {
		delete(starting, j.t)
		t, s := j.t, j.spec
		// --tracker 已在 addInput 阶段追加(磁力链要靠它拿元数据),此处不再重复。
		if j.isMagnet {
			if opt.publicTrackers {
				if priv := t.Info().Private; priv == nil || !*priv {
					t.AddTrackers([][]string{publicTrackers})
				}
			}
			saveMetaCache(t)
		}
		if !opt.jsonOutput {
			printTaskHeader(t)
		}
		sel := firstNonEmpty(s.Select, opt.selectFiles)
		filter := firstNonEmpty(s.SelectFilter, opt.selectFilter)
		var set map[int]bool
		if len(t.Files()) == 1 && (sel == "1" || sel == "0" || sel == "") && filter == "" {
			downloadAllFiles(t.Files())
			set = map[int]bool{1: true}
		} else {
			set = selectedSet(t, sel, filter)
			if !applySelectionSet(t, set) {
				fmt.Fprintf(os.Stderr, "[错误] %s: 没有文件被选中,已跳过(用 --all 查看编号)\n", t.Info().Name)
				emitError(s.Input, "no file selected")
				t.Drop()
				return
			}
		}
		// --priority-high:命中的文件提到 High,其余选中文件保持 Normal。
		if prio := firstNonEmpty(s.PriorityHigh, opt.priorityHigh); prio != "" {
			if n := applyHighPriority(t, prio, set); n == 0 {
				fmt.Fprintf(os.Stderr, "[警告] --priority-high %q 没有匹配任何文件\n", prio)
			} else if !opt.jsonOutput {
				fmt.Fprintf(out, "[优先级] %d 个文件优先下载: %s\n", n, prio)
			}
		}
		if opt.verify {
			fmt.Fprintf(out, "[校验] %s 正在重新校验已有数据...\n", t.Info().Name)
			if err := t.VerifyDataContext(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "[警告] %s 校验失败: %v\n", t.Info().Name, err)
			}
		}
		tk := &task{t: t, name: t.Info().Name, wanted: wantedBytes(t), lastSample: time.Now(), spec: s}
		tk.wantPieces = wantedPieceIndexes(t)
		// -o 既可以是 -i 行内参数,也可以是全局命令行参数;任务级优先。
		// 串流模式下忽略重命名:storage 仍按原 DisplayPath 读文件,改名会让后续
		// 读取(播放器 seek)直接失败。
		if rename := firstNonEmpty(s.Out, opt.out); rename != "" && len(t.Files()) == 1 {
			if opt.stream {
				fmt.Fprintln(os.Stderr, "[警告] --stream 模式下忽略 -o 重命名(流服务按原文件名读取)")
			} else {
				tk.renameOut = rename
			}
		}
		tasks = append(tasks, tk)
		// --stream:任务一激活就开服务,不必等下载完(Reader 会阻塞到数据可用)。
		if opt.stream {
			if f, err := pickStreamFile(tk, set); err != nil {
				fmt.Fprintf(os.Stderr, "[错误] 流服务: %v\n", err)
				emitError(s.Input, "stream: "+err.Error())
			} else if srv, err := startStream(ctx, f, opt.streamPort); err != nil {
				fmt.Fprintf(os.Stderr, "[错误] 启动流服务失败: %v\n", err)
				emitError(s.Input, "stream: "+err.Error())
			} else {
				fmt.Fprintf(out, "[流] 已就绪: %s\n", srv.url)
				fmt.Fprintln(out, "[流] 用播放器打开上面的地址,下载完成后服务保持,按 Ctrl+C 退出")
				emitJSON(jsonStatus{Event: "stream", Name: tk.name, URL: srv.url, State: "streaming"})
			}
		}
		if tk.wanted == 0 || tk.complete() {
			fmt.Fprintf(out, "[完成] %s 已存在,无需下载\n", tk.name)
			tk.done = true
			w := msgWriter()
			tk.applyRename(w)
			emitJSON(tk.statusLine("complete", countActive(tasks), len(pending)+len(starting)))
		} else {
			if opt.sequential {
				go sequentialWindow(ctx, tk.t, tk.wantPieces)
			}
			if p != nil {
				attachBar(p, tk)
			}
			emitJSON(tk.statusLine("start", countActive(tasks), len(pending)+len(starting)))
		}
		tl.setActive(tasks)
		writeSession()
	}

	fill()
loop:
	for len(pending) > 0 || len(starting) > 0 || countActive(tasks) > 0 {
		select {
		case <-ctx.Done():
			interrupted = true
			break loop
		case j := <-readyCh:
			activate(j)
			fill()
		case j := <-failCh:
			delete(starting, j.t)
			// 失败任务不再本次重试(否则会无限循环),但写进会话供 --resume 重试
			failed = append(failed, j.spec)
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "[警告] %s 获取元数据超时,保留在会话中(可用 --resume 重试)\n", j.t.InfoHash().HexString()[:16])
				emitError(j.spec.Input, "metadata timeout")
			}
			j.t.Drop()
			if ctx.Err() == nil {
				fill()
			}
		case <-tick.C:
			justDone := refreshTasks(tasks, time.Now())
			w := msgWriter()
			for _, tk := range justDone {
				tk.applyRename(w)
				tk.runOnComplete(ctx, w)
				emitJSON(tk.statusLine("complete", countActive(tasks), len(pending)+len(starting)))
			}
			for _, tk := range tasks {
				if tk.done {
					if !tk.barDone && tk.bar != nil {
						tk.bar.SetCurrent(tk.wanted) // 到量即自动完成并移除
						tk.barDone = true
					}
					continue
				}
				if tk.bar != nil {
					tk.bar.SetCurrent(wantedBytesDone(tk.t))
				}
				if opt.jsonOutput {
					emitJSON(tk.statusLine("progress", countActive(tasks), len(pending)+len(starting)))
				}
			}
			if len(justDone) > 0 {
				tl.setActive(tasks)
				writeSession()
				fill()
			}
			// 停滞检测:仍有活跃任务但连续 N 秒速度为 0 则结束,未完成项留在会话
			if opt.stallTimeout > 0 && countActive(tasks) > 0 {
				var speed float64
				for _, tk := range tasks {
					if !tk.done {
						speed += tk.speed
					}
				}
				if speed <= 0 {
					stallSecs++
				} else {
					stallSecs = 0
				}
				if stallSecs >= opt.stallTimeout {
					stalled = true
					break loop
				}
			}
		}
	}

	if p != nil {
		for _, tk := range tasks {
			if !tk.done && tk.bar != nil {
				tk.bar.Abort(true)
			}
		}
		p.Wait()
	}
	fmt.Fprintln(out)
	if stalled {
		fmt.Fprintf(os.Stderr, "[警告] 连续 %d 秒无下载速度,自动退出\n", stallSecs)
	}
	if opt.jsonOutput {
		emitJSON(jsonStatus{Event: "summary", State: "done", Active: 0, Queued: len(pending) + len(starting) + len(failed), Total: totalWanted(tasks)})
	} else {
		printSummary(tasks, time.Since(start))
	}
	if interrupted || stalled || len(failed) > 0 {
		writeSession()
		if len(failed) > 0 {
			fmt.Fprintf(os.Stderr, "[警告] %d 个任务未完成,已保留在会话中(可用 --resume 重试)\n", len(failed))
		}
		return
	}
	sessionWarn(saveSession(nil)) // 全部完成,清除会话
	if opt.seed && anySeeder(tasks) {
		waitSeeding(ctx, tasks)
	}
	// 边下边播:即使下载结束也保持进程与服务,直到用户 Ctrl+C。
	// 否则播放器刚打开就被断开,失去串流的意义。
	if opt.stream {
		fmt.Fprintln(out, "[流] 下载已完成,服务保持中;按 Ctrl+C 退出")
		<-ctx.Done()
	}
}

func metaContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if opt.metaTimeout > 0 {
		return context.WithTimeout(ctx, time.Duration(opt.metaTimeout)*time.Second)
	}
	return context.WithCancel(ctx)
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
