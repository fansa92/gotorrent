package main

// 任务引擎:CLI 与 RPC 控制面共用的调度核心。
//
// 职责边界:
//   - 引擎独占 torrent.Client 与全部任务状态,负责队列、并发上限、元数据
//     等待、选择规则应用、进度采样、暂停 / 恢复 / 移除。
//   - 传输层(RPC)只通过 Manager 的公开方法发命令、通过事件订阅收状态,
//     不直接触碰下载逻辑。
//
// 并发模型:所有状态变更都经 reqCh 进入主循环,主循环是唯一的状态写入点,
// 因此任务状态本身不需要加锁;外部调用通过 reply 通道同步拿到结果。
// 事件订阅者各自持有带缓冲的通道,满了就丢弃该事件(SSE 客户端可用
// tellStatus 拉权威快照兜底),绝不阻塞下载主循环。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/vbauerster/mpb/v8"
)

// ---------------------------------------------------------------------------
// 任务状态
// ---------------------------------------------------------------------------

// taskState 是任务在引擎内的状态。
type taskState string

const (
	stateQueued      taskState = "queued"      // 排队中,尚未加入客户端
	stateWaiting     taskState = "waiting"     // 已加入客户端,等待元数据
	stateDownloading taskState = "downloading" // 下载中
	statePaused      taskState = "paused"      // 已暂停(保留已完成分片)
	stateSeeding     taskState = "seeding"     // 下载完成,做种中
	stateComplete    taskState = "complete"    // 完成且不再做种
	stateError       taskState = "error"       // 失败
)

// ---------------------------------------------------------------------------
// 事件
// ---------------------------------------------------------------------------

// eventKind 是引擎向外广播的事件类型。
type eventKind string

const (
	evAdded    eventKind = "added"
	evStart    eventKind = "start"
	evProgress eventKind = "progress"
	evComplete eventKind = "complete"
	evPaused   eventKind = "paused"
	evResumed  eventKind = "resumed"
	evRemoved  eventKind = "removed"
	evError    eventKind = "error"
	evSeeding  eventKind = "seeding"
	evInfo     eventKind = "info"
)

// event 是一条引擎事件。task 是事件发生时的任务快照。
type event struct {
	kind eventKind
	task jsonStatus
}

// ---------------------------------------------------------------------------
// 请求 / 响应
// ---------------------------------------------------------------------------

type reqKind int

const (
	reqAdd reqKind = iota
	reqPause
	reqResume
	reqRemove
	reqList
	reqStatus
)

// req 是发给引擎主循环的命令。
type req struct {
	kind        reqKind
	id          string
	spec        taskSpec
	deleteFiles bool
	reply       chan reqResp
}

// reqResp 是命令结果。list/status 时 tasks 带上快照。
type reqResp struct {
	err   error
	id    string
	tasks []jsonStatus
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// Manager 是任务引擎。所有任务(含排队、暂停、已完成)都登记在 tasks 里,
// byID 用于按 ID 查找,queue 是尚未启动的任务队列。
type Manager struct {
	cl  *torrent.Client
	tl  *taskLimiter
	ctx context.Context

	// daemon 为真时主循环永不主动退出,只等外部(Ctrl+C / RPC shutdown)结束。
	daemon bool

	tasks []*task
	byID  map[string]*task
	queue []*task

	readyCh chan *task
	failCh  chan *task
	reqCh   chan req

	p *mpb.Progress

	// seedShown 保证"[做种中]"只提示一次。
	seedShown bool

	subsMu  sync.Mutex
	subs    map[uint64]chan event
	nextSub uint64

	done chan struct{}
}

// newManager 创建引擎。调用方需先 Start 再发命令。
func newManager(ctx context.Context, cl *torrent.Client, tl *taskLimiter) *Manager {
	return &Manager{
		cl:      cl,
		tl:      tl,
		ctx:     ctx,
		tasks:   []*task{},
		byID:    map[string]*task{},
		queue:   []*task{},
		readyCh: make(chan *task, 16),
		failCh:  make(chan *task, 16),
		reqCh:   make(chan req, 32),
		subs:    map[uint64]chan event{},
		done:    make(chan struct{}),
	}
}

// Start 启动引擎主循环(后台 goroutine)。
func (m *Manager) Start() {
	go m.loop()
}

// Wait 等待引擎主循环退出。
func (m *Manager) Wait() { <-m.done }

// ---------------------------------------------------------------------------
// 对外命令接口(阻塞直到主循环处理完毕)
// ---------------------------------------------------------------------------

// Add 登记一个新任务并入队。返回任务 ID(infohash 十六进制)。
// 同一个 infohash 重复添加会被拒绝,返回已有任务 ID 与错误。
func (m *Manager) Add(spec taskSpec) (string, error) {
	id, err := taskIDOf(spec.Input)
	if err != nil {
		return "", err
	}
	reply := make(chan reqResp, 1)
	m.reqCh <- req{kind: reqAdd, id: id, spec: spec, reply: reply}
	r := <-reply
	return r.id, r.err
}

// AddAll 批量登记初始任务,逐个返回结果(失败不中断)。
func (m *Manager) AddAll(specs []taskSpec) {
	for _, s := range specs {
		if _, err := m.Add(s); err != nil {
			fmt.Fprintf(os.Stderr, "[错误] 添加任务失败 %s: %v\n", s.Input, err)
			emitError(s.Input, err.Error())
		}
	}
}

// Pause 暂停任务。排队中的任务直接停住,下载中的任务取消文件优先级
// (保留已完成分片,恢复后继续)。
func (m *Manager) Pause(id string) error {
	reply := make(chan reqResp, 1)
	m.reqCh <- req{kind: reqPause, id: id, reply: reply}
	return (<-reply).err
}

// Resume 恢复被暂停的任务。
func (m *Manager) Resume(id string) error {
	reply := make(chan reqResp, 1)
	m.reqCh <- req{kind: reqResume, id: id, reply: reply}
	return (<-reply).err
}

// Remove 移除任务。deleteFiles 为真时连同已下载的数据一起删除。
func (m *Manager) Remove(id string, deleteFiles bool) error {
	reply := make(chan reqResp, 1)
	m.reqCh <- req{kind: reqRemove, id: id, deleteFiles: deleteFiles, reply: reply}
	return (<-reply).err
}

// List 返回全部任务快照。
func (m *Manager) List() []jsonStatus {
	reply := make(chan reqResp, 1)
	m.reqCh <- req{kind: reqList, reply: reply}
	return (<-reply).tasks
}

// Status 返回单个任务快照。
func (m *Manager) Status(id string) (jsonStatus, error) {
	reply := make(chan reqResp, 1)
	m.reqCh <- req{kind: reqStatus, id: id, reply: reply}
	r := <-reply
	if r.err != nil {
		return jsonStatus{}, r.err
	}
	if len(r.tasks) == 0 {
		return jsonStatus{}, fmt.Errorf("任务不存在: %s", id)
	}
	return r.tasks[0], nil
}

// Subscribe 订阅引擎事件。返回事件通道与取消函数。
// 订阅者应尽快消费:通道满时新事件会被丢弃,避免拖慢下载主循环。
func (m *Manager) Subscribe() (<-chan event, func()) {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	id := m.nextSub
	m.nextSub++
	ch := make(chan event, 256)
	m.subs[id] = ch
	return ch, func() {
		m.subsMu.Lock()
		defer m.subsMu.Unlock()
		delete(m.subs, id)
		close(ch)
	}
}

// emit 向所有订阅者广播事件。非阻塞:订阅者来不及消费就丢弃。
func (m *Manager) emit(kind eventKind, tk *task) {
	if tk == nil {
		return
	}
	ev := event{kind: kind, task: tk.snapshot()}
	// 活跃数 / 排队数在主循环里算好再带上,订阅者不用再回查引擎。
	ev.task.Active = m.countActive()
	ev.task.Queued = len(m.queue)
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for _, ch := range m.subs {
		select {
		case ch <- ev:
		default: // 订阅者太慢,丢弃该事件
		}
	}
}

// ---------------------------------------------------------------------------
// 主循环
// ---------------------------------------------------------------------------

func (m *Manager) loop() {
	defer close(m.done)

	if !opt.jsonOutput {
		// 刷新频率与数据采样(1s)对齐。WithAutoRefresh 是必须的:
		// mpb 在输出不是终端(重定向/管道)时会进入 noRenderMode,既不画进度条,
		// 也会把通过 p.Write() 输出的重命名/钩子消息永远留在缓冲区里丢失。
		// 开启后:终端里仍是原地刷新;非终端则每个周期追加一行纯文本进度,
		// 日志可读且消息不会丢。
		m.p = mpb.New(mpb.WithWidth(100), mpb.WithRefreshRate(time.Second), mpb.WithAutoRefresh(), mpb.WithOutput(out))
	}

	m.writeSession()

	start := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	stallSecs := 0

	for {
		select {
		case <-m.ctx.Done():
			m.finish(time.Since(start), true, false, stallSecs)
			return

		case r := <-m.reqCh:
			m.handleReq(r)

		case tk := <-m.readyCh:
			m.activate(tk)
			m.fill()

		case tk := <-m.failCh:
			m.failTask(tk)
			m.fill()

		case <-tick.C:
			now := time.Now()
			justDone := m.refreshTasks(now)
			w := m.msgWriter()
			for _, tk := range justDone {
				tk.applyRename(w)
				tk.runOnComplete(m.ctx, w)
				m.emit(evComplete, tk)
			}
			m.checkSeedLimits(now)
			m.printSeedingStatus(now)
			m.updateBars()
			for _, tk := range m.tasks {
				switch tk.state {
				case stateDownloading, stateWaiting:
					m.emit(evProgress, tk)
				}
			}
			if len(justDone) > 0 {
				m.tl.setActive(m.tasks)
				m.writeSession()
				m.fill()
			}
			// 停滞检测:仍有活跃任务但连续 N 秒速度为 0 则结束,未完成项留在会话
			if opt.stallTimeout > 0 && m.countActive() > 0 {
				var speed float64
				for _, tk := range m.tasks {
					if tk.state == stateDownloading {
						speed += tk.speed
					}
				}
				if speed <= 0 {
					stallSecs++
				} else {
					stallSecs = 0
				}
				if stallSecs >= opt.stallTimeout {
					m.finish(time.Since(start), false, true, stallSecs)
					return
				}
			}
			// 非守护模式:没有待办也没有在做种就收工退出。
			// 守护模式永不主动退出,只等外部(Ctrl+C / RPC shutdown)结束。
			if !m.daemon && !m.hasWork() {
				m.finish(time.Since(start), false, false, stallSecs)
				return
			}
		}
	}
}

// hasWork 判断是否还有待处理的工作(排队 / 等元数据 / 下载中 / 做种中)。
// 暂停的任务不算待办:否则 CLI 模式会一直挂着等一个永远不会来的恢复指令。
func (m *Manager) hasWork() bool {
	if len(m.queue) > 0 {
		return true
	}
	for _, tk := range m.tasks {
		switch tk.state {
		case stateQueued, stateWaiting, stateDownloading, stateSeeding:
			return true
		}
	}
	return false
}

// updateBars 刷新进度条。已完成的任务把进度推到顶并移除进度条。
func (m *Manager) updateBars() {
	for _, tk := range m.tasks {
		if tk.bar == nil {
			continue
		}
		if tk.done {
			if !tk.barDone {
				tk.bar.SetCurrent(tk.wanted) // 到量即自动完成并移除
				tk.barDone = true
			}
			continue
		}
		tk.bar.SetCurrent(tk.wantedBytesDone())
	}
}

// checkSeedLimits 检查做种任务是否达到分享率 / 时长上限,达到就停止做种。
func (m *Manager) checkSeedLimits(now time.Time) {
	for _, tk := range m.tasks {
		if tk.state != stateSeeding || tk.t == nil {
			continue
		}
		up := tk.t.Stats().BytesWrittenData
		upBytes := up.Int64()
		if opt.seedTime > 0 && now.Sub(tk.seedStart) >= time.Duration(opt.seedTime*float64(time.Minute)) {
			m.stopSeeding(tk, fmt.Sprintf("达到做种时长上限(%g 分钟)", opt.seedTime))
			continue
		}
		if opt.seedRatio > 0 && tk.wanted > 0 && float64(upBytes) >= opt.seedRatio*float64(tk.wanted) {
			m.stopSeeding(tk, fmt.Sprintf("达到分享率上限(%.2f)", opt.seedRatio))
		}
	}
}

// stopSeeding 停止一个任务的做种。
func (m *Manager) stopSeeding(tk *task, reason string) {
	if !opt.jsonOutput {
		fmt.Fprintf(out, "[做种结束] %s: %s\n", trunc(tk.name, 40), reason)
	}
	tk.t.Drop()
	tk.state = stateComplete
	m.emit(evComplete, tk)
	m.writeSession()
}

// printSeedingStatus 打印做种状态(人读模式)。
func (m *Manager) printSeedingStatus(now time.Time) {
	if opt.jsonOutput {
		return
	}
	var up, peers, wanted int64
	seeding := 0
	var started time.Time
	for _, tk := range m.tasks {
		if tk.state != stateSeeding || tk.t == nil {
			continue
		}
		seeding++
		st := tk.t.Stats()
		up += st.BytesWrittenData.Int64()
		peers += int64(st.ActivePeers)
		wanted += tk.wanted
		if started.IsZero() || tk.seedStart.Before(started) {
			started = tk.seedStart
		}
	}
	if seeding == 0 {
		return
	}
	if !m.seedShown {
		m.seedShown = true
		fmt.Fprintln(out, "\n[做种中] 按 Ctrl+C 退出")
		printSeedLimits()
	}
	ratio := 0.0
	if wanted > 0 {
		ratio = float64(up) / float64(wanted)
	}
	fmt.Fprintf(out, "\r[做种] 已上传 %s | 对等点 %d | 分享率 %.2f | 已运行 %s%12s",
		human(up), peers, ratio, now.Sub(started).Round(time.Second), "")
}

// finish 收尾:汇总输出、写会话。
func (m *Manager) finish(dur time.Duration, interrupted, stalled bool, stallSecs int) {
	if m.p != nil {
		for _, tk := range m.tasks {
			if !tk.done && tk.bar != nil {
				tk.bar.Abort(true)
			}
		}
		m.p.Wait()
	}
	fmt.Fprintln(out)
	if stalled {
		fmt.Fprintf(os.Stderr, "[警告] 连续 %d 秒无下载速度,自动退出\n", stallSecs)
	}
	if interrupted {
		fmt.Fprintln(os.Stderr, "[提示] 已中断,未完成任务保留在会话中(用 --resume 继续)")
	}
	if opt.jsonOutput {
		emitJSON(jsonStatus{Event: "summary", State: "done", Active: 0, Queued: len(m.queue), Total: m.totalWanted()})
	} else {
		printSummary(m.tasks, dur)
	}
	m.writeSession()
}

// handleReq 处理一条来自传输层的命令。
func (m *Manager) handleReq(r req) {
	resp := reqResp{id: r.id}
	switch r.kind {
	case reqAdd:
		resp.id, resp.err = m.addTask(r)
	case reqPause:
		resp.err = m.pauseTask(r.id)
	case reqResume:
		resp.err = m.resumeTask(r.id)
	case reqRemove:
		resp.err = m.removeTask(r.id, r.deleteFiles)
	case reqList:
		resp.tasks = m.snapshots()
	case reqStatus:
		if tk, ok := m.byID[r.id]; ok {
			resp.tasks = []jsonStatus{tk.snapshot()}
		}
	}
	if r.reply != nil {
		r.reply <- resp
	}
}

// addTask 登记任务并入队。infohash 相同的任务视为重复。
func (m *Manager) addTask(r req) (string, error) {
	if r.id == "" {
		return "", fmt.Errorf("无法解析输入 %q 的 infohash", r.spec.Input)
	}
	if old, ok := m.byID[r.id]; ok {
		// 重复添加:已存在就返回原 ID,不算失败。
		return old.id, nil
	}
	spec := r.spec
	if !isMagnet(spec.Input) && !strings.Contains(spec.Input, "://") {
		spec.Input = mustAbs(spec.Input)
	}
	tk := &task{
		id:      r.id,
		spec:    spec,
		state:   stateQueued,
		created: time.Now(),
	}
	m.tasks = append(m.tasks, tk)
	m.byID[r.id] = tk
	if spec.Paused {
		// 从会话恢复的暂停任务:只登记不启动,等 resume 指令。
		tk.state = statePaused
	} else {
		m.queue = append(m.queue, tk)
	}
	m.emit(evAdded, tk)
	m.fill()
	m.writeSession()
	return r.id, nil
}

// pauseTask 暂停任务。排队中的直接出队,下载中的取消文件优先级。
func (m *Manager) pauseTask(id string) error {
	tk, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("任务不存在: %s", id)
	}
	switch tk.state {
	case statePaused:
		return nil
	case stateQueued:
		m.removeFromQueue(tk)
		tk.state = statePaused
		m.emit(evPaused, tk)
		m.writeSession()
		return nil
	case stateDownloading, stateWaiting:
		// anacrolix v1.61 的 *Torrent 没有 Pause 方法,暂停的语义就是
		// 把所有文件的优先级降到 None:客户端不再请求任何分片,但连接、
		// 已完成分片和断点续传状态全部保留。
		if tk.t != nil && tk.t.Info() != nil {
			for _, f := range tk.t.Files() {
				f.SetPriority(torrent.PiecePriorityNone)
			}
		}
		tk.state = statePaused
		m.emit(evPaused, tk)
		m.tl.setActive(m.tasks)
		m.writeSession()
		return nil
	default:
		return fmt.Errorf("任务 %s 当前状态 %s 不可暂停", id, tk.state)
	}
}

// resumeTask 恢复任务。重新套用选择规则与优先级。
func (m *Manager) resumeTask(id string) error {
	tk, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("任务不存在: %s", id)
	}
	if tk.state != statePaused {
		return fmt.Errorf("任务 %s 当前状态 %s 不可恢复", id, tk.state)
	}
	if tk.t == nil {
		// 还没加入客户端(暂停发生在排队阶段),重新排队即可。
		tk.state = stateQueued
		m.queue = append(m.queue, tk)
		m.emit(evResumed, tk)
		m.fill()
		m.writeSession()
		return nil
	}
	if tk.t.Info() == nil {
		// 元数据还没到,等它到了再由 activate 套用选择规则。
		tk.state = stateWaiting
		m.emit(evResumed, tk)
		return nil
	}
	m.applySelection(tk)
	tk.state = stateDownloading
	tk.done = false
	m.emit(evResumed, tk)
	m.tl.setActive(m.tasks)
	m.fill()
	m.writeSession()
	return nil
}

// removeTask 移除任务,可选删除已下载的数据。
func (m *Manager) removeTask(id string, deleteFiles bool) error {
	tk, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("任务不存在: %s", id)
	}
	m.removeFromQueue(tk)
	if tk.t != nil {
		if deleteFiles {
			m.deleteTaskData(tk)
		}
		tk.t.Drop()
	}
	// 从任务表里摘掉
	kept := m.tasks[:0]
	for _, x := range m.tasks {
		if x != tk {
			kept = append(kept, x)
		}
	}
	m.tasks = kept
	delete(m.byID, id)
	m.emit(evRemoved, tk)
	m.tl.setActive(m.tasks)
	m.writeSession()
	return nil
}

// deleteTaskData 删除任务已下载的数据文件(仅删除被选中的文件)。
func (m *Manager) deleteTaskData(tk *task) {
	if tk.t == nil || tk.t.Info() == nil {
		return
	}
	base := mustAbs(opt.dir)
	for _, f := range tk.t.Files() {
		p := filepath.Join(base, filepath.FromSlash(f.DisplayPath()))
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[警告] 删除文件失败 %s: %v\n", p, err)
		}
	}
}

// removeFromQueue 把任务从排队队列里摘掉。
func (m *Manager) removeFromQueue(tk *task) {
	for i, x := range m.queue {
		if x == tk {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// 队列与激活
// ---------------------------------------------------------------------------

// limit 返回同时活跃任务数的上限。0 表示不限。
func (m *Manager) limit() int {
	if opt.maxConcurrent <= 0 {
		return 1 << 30
	}
	return opt.maxConcurrent
}

// countActive 统计正在占用下载资源的任务(排队中的不算)。
func (m *Manager) countActive() int {
	n := 0
	for _, tk := range m.tasks {
		switch tk.state {
		case stateWaiting, stateDownloading:
			n++
		}
	}
	return n
}

// fill 把排队任务补进空位。
func (m *Manager) fill() {
	for len(m.queue) > 0 && m.countActive() < m.limit() {
		tk := m.queue[0]
		m.queue = m.queue[1:]
		t, err := addInput(m.cl, tk.spec.Input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[错误] 添加任务失败 %s: %v\n", tk.spec.Input, err)
			emitError(tk.spec.Input, err.Error())
			tk.state = stateError
			tk.err = err.Error()
			m.emit(evError, tk)
			continue
		}
		tk.t = t
		tk.state = stateWaiting
		if !opt.jsonOutput {
			fmt.Fprintf(out, "[任务] 已添加: %s\n", describeInput(tk.spec.Input, t))
		}
		go m.awaitMeta(tk)
	}
}

// awaitMeta 等待元数据就绪。
func (m *Manager) awaitMeta(tk *task) {
	metaCtx, cancel := metaContext(m.ctx)
	defer cancel()
	select {
	case <-tk.t.GotInfo():
		m.readyCh <- tk
	case <-metaCtx.Done():
		m.failCh <- tk
	}
}

// failTask 处理元数据获取失败。
func (m *Manager) failTask(tk *task) {
	tk.state = stateError
	tk.err = "metadata timeout"
	if m.ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "[警告] %s 获取元数据超时,保留在会话中(可用 --resume 重试)\n", tk.id[:16])
		emitError(tk.spec.Input, "metadata timeout")
	}
	tk.t.Drop()
	m.emit(evError, tk)
}

// activate 元数据就绪:应用选择、校验、建 task/进度条、启动顺序下载。
func (m *Manager) activate(tk *task) {
	t := tk.t
	s := tk.spec
	// --tracker 已在 addInput 阶段追加(磁力链要靠它拿元数据),此处不再重复。
	if isMagnet(s.Input) {
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
			tk.state = stateError
			tk.err = "no file selected"
			m.emit(evError, tk)
			return
		}
	}
	// --priority-high:命中的文件提到 High,其余选中文件保持 Normal。
	if prio := firstNonEmpty(s.PriorityHigh, opt.priorityHigh); prio != "" {
		// 先算出命中集合并缓存:暂停后恢复要按同一套优先级重建,
		// 靠实时优先级反推会丢失"哪些文件是高优先级"的信息。
		tk.highSet = filesBySpec(t, prio)
		if n := applyHighPriority(t, prio, set); n == 0 {
			fmt.Fprintf(os.Stderr, "[警告] --priority-high %q 没有匹配任何文件\n", prio)
		} else if !opt.jsonOutput {
			fmt.Fprintf(out, "[优先级] %d 个文件优先下载: %s\n", n, prio)
		}
	}
	if opt.verify {
		fmt.Fprintf(out, "[校验] %s 正在重新校验已有数据...\n", t.Info().Name)
		if err := t.VerifyDataContext(m.ctx); err != nil && m.ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "[警告] %s 校验失败: %v\n", t.Info().Name, err)
		}
	}

	tk.name = t.Info().Name
	tk.selected = set
	tk.selFiles = filesBySet(t, set)
	tk.wanted = lengthOfFiles(tk.selFiles)
	tk.wantPieces = pieceIndexesOfFiles(tk.selFiles)
	tk.lastSample = time.Now()

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
	// --stream:任务一激活就开服务,不必等下载完(Reader 会阻塞到数据可用)。
	if opt.stream {
		if f, err := pickStreamFile(tk, set); err != nil {
			fmt.Fprintf(os.Stderr, "[错误] 流服务: %v\n", err)
			emitError(s.Input, "stream: "+err.Error())
		} else if srv, err := startStream(m.ctx, f, opt.streamPort); err != nil {
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
		tk.state = stateComplete
		w := m.msgWriter()
		tk.applyRename(w)
		m.emit(evComplete, tk)
		return
	}
	tk.state = stateDownloading
	if opt.sequential {
		go sequentialWindow(m.ctx, tk.t, tk.wantPieces)
	}
	if m.p != nil {
		attachBar(m.p, tk)
	}
	m.emit(evStart, tk)
	m.tl.setActive(m.tasks)
	m.writeSession()
}

// applySelection 重新套用选择规则与优先级(暂停后恢复用)。
func (m *Manager) applySelection(tk *task) {
	if tk.t == nil || tk.t.Info() == nil {
		return
	}
	applySelectionSet(tk.t, tk.selected)
	for i, f := range tk.t.Files() {
		idx := i + 1
		switch {
		case tk.highSet != nil && tk.highSet[idx]:
			f.SetPriority(torrent.PiecePriorityHigh)
		case tk.selected[idx]:
			f.SetPriority(torrent.PiecePriorityNormal)
		default:
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}
}

// ---------------------------------------------------------------------------
// 进度与会话
// ---------------------------------------------------------------------------

// refreshTasks 采样速度并判定新完成的任务。
func (m *Manager) refreshTasks(now time.Time) (justDone []*task) {
	for _, tk := range m.tasks {
		if tk.t == nil {
			continue
		}
		st := tk.t.Stats()
		read := st.BytesReadUsefulData.Int64()
		write := st.BytesWrittenData.Int64()
		dt := now.Sub(tk.lastSample).Seconds()
		if dt > 0 {
			tk.speed = delta(read, tk.prevRead) / dt
			tk.upSpeed = delta(write, tk.prevWrite) / dt
		}
		tk.prevRead, tk.prevWrite, tk.lastSample = read, write, now
		if !tk.done && tk.state == stateDownloading && tk.complete() {
			tk.done = true
			tk.seedStart = now
			if opt.seed {
				// 下载完成但继续做种:任务转入做种状态,主循环会一直等它
				// 达到分享率/时长上限(或被 Ctrl+C 打断)。
				tk.state = stateSeeding
				m.emit(evSeeding, tk)
			} else {
				tk.state = stateComplete
			}
			justDone = append(justDone, tk)
		}
	}
	return
}

// writeSession 记录"仍未完成"的任务:排队中 + 等待元数据 + 下载中 + 暂停中。
func (m *Manager) writeSession() {
	var remain []taskSpec
	for _, tk := range m.tasks {
		switch tk.state {
		case stateComplete:
			continue
		}
		s := tk.spec
		s.Paused = tk.state == statePaused
		remain = append(remain, s)
	}
	sessionWarn(saveSession(remain))
}

// snapshots 返回全部任务快照。m.tasks 是追加式的,天然按创建时间有序。
func (m *Manager) snapshots() []jsonStatus {
	out := make([]jsonStatus, 0, len(m.tasks))
	for _, tk := range m.tasks {
		out = append(out, tk.snapshot())
	}
	return out
}

// totalWanted 统计所有任务的下载总量。
func (m *Manager) totalWanted() int64 {
	var n int64
	for _, tk := range m.tasks {
		n += tk.wanted
	}
	return n
}

// msgWriter 返回进度消息的写入目标。
func (m *Manager) msgWriter() io.Writer {
	if m.p != nil {
		return m.p
	}
	return out
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// metaContext 返回带元数据超时的上下文。超时<=0 时不限时。
func metaContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if opt.metaTimeout > 0 {
		return context.WithTimeout(ctx, time.Duration(opt.metaTimeout)*time.Second)
	}
	return context.WithCancel(ctx)
}

// taskIDOf 离线计算输入对应的 infohash,作为任务 ID。
//
// magnet 从链接解析、.torrent 直接读文件——两者都不需要联网,也不需要先
// 加入客户端,因此在入队前就能拿到稳定 ID(跨重启不变,天然可用于去重)。
func taskIDOf(input string) (string, error) {
	if isMagnet(input) {
		m, err := metainfo.ParseMagnetUri(strings.TrimSpace(input))
		if err != nil {
			return "", fmt.Errorf("解析磁力链接失败: %w", err)
		}
		return m.InfoHash.HexString(), nil
	}
	if strings.Contains(input, "://") {
		return "", fmt.Errorf("不支持的输入: %s", input)
	}
	mi, err := metainfo.LoadFromFile(input)
	if err != nil {
		return "", fmt.Errorf("读取种子文件失败: %w", err)
	}
	return mi.HashInfoBytes().HexString(), nil
}

// filesBySet 按 1-based 文件编号集合取出对应文件。
func filesBySet(t *torrent.Torrent, set map[int]bool) []*torrent.File {
	files := t.Files()
	out := make([]*torrent.File, 0, len(set))
	for i, f := range files {
		if set[i+1] {
			out = append(out, f)
		}
	}
	return out
}

// lengthOfFiles 统计文件总长度。
func lengthOfFiles(files []*torrent.File) int64 {
	var n int64
	for _, f := range files {
		n += f.Length()
	}
	return n
}

// pieceIndexesOfFiles 返回这些文件覆盖的分片下标(升序去重)。
func pieceIndexesOfFiles(files []*torrent.File) []int {
	seen := map[int]bool{}
	var out []int
	for _, f := range files {
		for i := f.BeginPieceIndex(); i < f.EndPieceIndex(); i++ {
			if !seen[i] {
				seen[i] = true
				out = append(out, i)
			}
		}
	}
	return out
}
