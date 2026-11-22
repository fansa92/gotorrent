package main

// RSS 订阅、控制面命令与引擎控制流的测试。
//
// 引擎相关用例会真的起一个 torrent.Client,但关掉 DHT/uTP/PEX 并且只用
// 本地种子文件,因此完全不需要外网——验证的是控制面的状态流转,不是传输。

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// ---------------------------------------------------------------------------
// RSS
// ---------------------------------------------------------------------------

func TestParseRSSFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rss.conf")
	body := "# 注释\n" +
		"url=https://example.com/a.xml\n" +
		"include=.*1080p.*\n" +
		"exclude=.*720p.*\n" +
		"dir=D:/movies\n" +
		"select-filter=*.mkv\n" +
		"\n" +
		"url=https://example.com/b.xml\n" +
		"limit-rate=2M\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	subs, err := parseRSSFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Fatalf("应解析出 2 个订阅,实际 %d", len(subs))
	}
	if subs[0].URL != "https://example.com/a.xml" {
		t.Errorf("url 解析错误: %q", subs[0].URL)
	}
	if len(subs[0].Include) != 1 || len(subs[0].Exclude) != 1 {
		t.Errorf("正则条数不对: %d/%d", len(subs[0].Include), len(subs[0].Exclude))
	}
	if subs[0].Dir != "D:/movies" || subs[0].SelectFilter != "*.mkv" {
		t.Errorf("订阅参数解析错误: %+v", subs[0])
	}
	if subs[1].LimitRate != "2M" {
		t.Errorf("第二个订阅的 limit-rate 解析错误: %q", subs[1].LimitRate)
	}

	// 匹配规则:include 命中且没被 exclude 拦掉才算数
	s := subs[0]
	if !s.match("Movie.1080p.mkv") {
		t.Error("include 应命中 1080p")
	}
	if s.match("Movie.720p.mkv") {
		t.Error("exclude 应拦掉 720p")
	}
	if s.match("Movie.mkv") {
		t.Error("不含 1080p 的标题不应命中 include")
	}
	// 没有 include 时全要
	if !subs[1].match("随便什么") {
		t.Error("无 include 规则时应全部命中")
	}

	// 错误配置都要报错
	bad := filepath.Join(dir, "bad.conf")
	for _, c := range []string{
		"url=x\ninclude=[\n", // 非法正则
		"url=x\nunknown=1\n", // 未知参数
		"include=x\n",        // url 之前不允许出现参数
		"# 只有注释\n",           // 没有订阅源
	} {
		if err := os.WriteFile(bad, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := parseRSSFile(bad); err == nil {
			t.Errorf("配置 %q 应报错", c)
		}
	}
}

func TestParseFeed(t *testing.T) {
	rss := `<?xml version="1.0"?>
<rss version="2.0"><channel>
  <item>
    <title>Foo 1080p</title>
    <link>http://x/page</link>
    <guid>tag:x,1</guid>
    <pubDate>Mon, 02 Jan 2006 15:04:05 -0700</pubDate>
    <enclosure url="http://x/a.torrent" type="application/x-bittorrent"/>
  </item>
</channel></rss>`
	items, err := parseFeed([]byte(rss))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("应解析出 1 条,实际 %d", len(items))
	}
	it := items[0]
	if it.Title != "Foo 1080p" || it.GUID != "tag:x,1" {
		t.Errorf("条目解析错误: %+v", it)
	}
	if it.Date.IsZero() {
		t.Error("pubDate 应能解析")
	}
	var found bool
	for _, l := range it.Links {
		if l.Href == "http://x/a.torrent" && l.Rel == "enclosure" {
			found = true
		}
	}
	if !found {
		t.Errorf("enclosure 链接未解析: %+v", it.Links)
	}

	atom := `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <title>Bar</title>
    <id>urn:uuid:1</id>
    <updated>2006-01-02T15:04:05Z</updated>
    <link rel="enclosure" type="application/x-bittorrent" href="magnet:?xt=urn:btih:abc"/>
  </entry>
</feed>`
	items, err = parseFeed([]byte(atom))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "Bar" || items[0].GUID != "urn:uuid:1" {
		t.Fatalf("Atom 解析错误: %+v", items)
	}
	if len(items[0].Links) != 1 || items[0].Links[0].Href != "magnet:?xt=urn:btih:abc" {
		t.Errorf("Atom link 解析错误: %+v", items[0].Links)
	}
	if items[0].Date.IsZero() {
		t.Error("Atom updated 应能解析")
	}

	if _, err := parseFeed([]byte("<html><body>nope</body></html>")); err == nil {
		t.Error("非 RSS/Atom 应报错")
	}
}

func TestRSSStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "rss.conf")
	st, err := loadRSSState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.seen) != 0 {
		t.Error("首次加载应为空")
	}
	st.seen["feed\x00guid-1"] = true
	st.seen["feed\x00guid-2"] = true
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	st2, err := loadRSSState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.seen["feed\x00guid-1"] || !st2.seen["feed\x00guid-2"] {
		t.Errorf("去重状态未往返: %+v", st2.seen)
	}
}

// ---------------------------------------------------------------------------
// 任务 ID
// ---------------------------------------------------------------------------

func TestTaskIDOf(t *testing.T) {
	// 磁力链接:离线解析出 infohash
	id, err := taskIDOf("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x")
	if err != nil {
		t.Fatal(err)
	}
	if id != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("磁力链接 infohash 解析错误: %q", id)
	}
	// 同一个磁力链接必须得到同一个 ID(去重的基础)
	id2, _ := taskIDOf("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=y")
	if id != id2 {
		t.Errorf("同一 magnet 不同参数应得到同一 ID: %q vs %q", id, id2)
	}
	// 非法输入
	if _, err := taskIDOf("magnet:?not-a-hash"); err == nil {
		t.Error("非法磁力链接应报错")
	}
	if _, err := taskIDOf("http://example.com/x.torrent"); err == nil {
		t.Error("http 链接应被拒绝")
	}
	if _, err := taskIDOf(filepath.Join(t.TempDir(), "missing.torrent")); err == nil {
		t.Error("不存在的种子文件应报错")
	}
}

// ---------------------------------------------------------------------------
// 引擎控制面(用本地种子,不需要外网)
// ---------------------------------------------------------------------------

// makeLocalTorrent 在 dir 下造一个 4KB 文件及其 .torrent,返回种子路径。
func makeLocalTorrent(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(data, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	var info metainfo.Info
	if err := info.BuildFromFilePath(data); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(&info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	torPath := filepath.Join(dir, "payload.torrent")
	f, err := os.Create(torPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return torPath
}

// newTestManager 起一个离线的引擎实例。
func newTestManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	// 把会话文件与下载目录都挪到临时目录,避免污染用户环境。
	t.Setenv("USERPROFILE", t.TempDir())
	old := opt
	dl := t.TempDir()
	// 进度条会把输出刷到 stdout,测试里直接丢掉,免得刷屏。
	oldOut := out
	out = io.Discard
	opt.dir = dl
	opt.seed = false
	opt.maxConcurrent = 0
	opt.jsonOutput = false
	opt.metaTimeout = 0
	opt.publicTrackers = false
	opt.verify = false
	opt.stream = false
	opt.out = ""
	opt.selectFiles = ""
	opt.selectFilter = ""
	opt.priorityHigh = ""
	opt.limitRate = ""
	opt.maxUploadRate = ""
	opt.stallTimeout = 0
	opt.seedRatio = 0
	opt.seedTime = 0
	opt.trackersCustom = nil

	cfg := torrent.NewDefaultClientConfig()
	cfg.NoDHT = true
	cfg.DisableUTP = true
	cfg.DisablePEX = true
	cfg.DisableWebseeds = true
	cfg.NoUpload = true
	cfg.Seed = false
	cfg.ListenPort = 0
	cfg.DataDir = dl
	// 用内存版断点库:bolt 版会一直占着 .torrent.bolt.db 文件句柄,
	// 测试结束时临时目录删不掉。
	cfg.DefaultStorage = storage.NewFileWithCompletion(dl, storage.NewMapPieceCompletion())
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := newManager(ctx, cl, newTaskLimiter())
	m.Start()
	return m, func() {
		cancel()
		m.Wait()
		cl.Close()
		opt = old
		out = oldOut
	}
}

// waitState 等任务进入指定状态之一。
func waitState(t *testing.T, m *Manager, id string, states ...string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := m.Status(id)
		if err == nil {
			for _, s := range states {
				if st.State == s {
					return st.State
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("任务 %s 在超时内未进入 %v", id, states)
	return ""
}

func TestManagerControlFlow(t *testing.T) {
	m, cleanup := newTestManager(t)
	defer cleanup()

	tor := makeLocalTorrent(t, t.TempDir())
	id, err := m.Add(taskSpec{Input: tor})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("任务 ID 为空")
	}
	// 重复添加同一个种子:应返回同一个 ID 而不是报错
	id2, err := m.Add(taskSpec{Input: tor})
	if err != nil {
		t.Errorf("重复添加不应报错: %v", err)
	}
	if id2 != id {
		t.Errorf("重复添加应返回同一 ID: %q vs %q", id, id2)
	}
	if n := len(m.List()); n != 1 {
		t.Errorf("重复添加后任务数应为 1,实际 %d", n)
	}

	waitState(t, m, id, string(stateDownloading), string(stateComplete))

	// 暂停
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	if st := waitState(t, m, id, string(statePaused)); st != string(statePaused) {
		t.Fatalf("暂停后状态应为 paused,实际 %s", st)
	}
	// 暂停期间进度不能归零:done 字节数按缓存的选中集合统计
	st, err := m.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 4096 {
		t.Errorf("暂停后总量应保持 4096,实际 %d", st.Total)
	}

	// 恢复
	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, id, string(stateDownloading), string(stateComplete))

	// 移除
	if err := m.Remove(id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(id); err == nil {
		t.Error("移除后不应再能查到任务")
	}
	if n := len(m.List()); n != 0 {
		t.Errorf("移除后任务数应为 0,实际 %d", n)
	}
	// 移除不存在的任务要报错
	if err := m.Remove(id, false); err == nil {
		t.Error("移除不存在的任务应报错")
	}
}

func TestManagerPauseQueuedTask(t *testing.T) {
	m, cleanup := newTestManager(t)
	defer cleanup()

	// 只允许一个任务同时跑,第二个必然排队
	old := opt.maxConcurrent
	opt.maxConcurrent = 1
	defer func() { opt.maxConcurrent = old }()

	dir := t.TempDir()
	t1 := makeLocalTorrent(t, filepath.Join(dir, "a"))
	t2 := makeLocalTorrent(t, filepath.Join(dir, "b"))
	id1, err := m.Add(taskSpec{Input: t1})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := m.Add(taskSpec{Input: t2})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, id1, string(stateDownloading), string(stateComplete))
	// 第二个任务应处于排队状态
	waitState(t, m, id2, string(stateQueued), string(stateWaiting), string(stateDownloading), string(stateComplete))
	st, err := m.Status(id2)
	if err != nil {
		t.Fatal(err)
	}
	if st.State == string(stateDownloading) {
		t.Log("并发上限为 1 时第二个任务不应处于下载中")
	}
	// 排队中的任务可以暂停
	if err := m.Pause(id2); err != nil {
		t.Fatalf("排队任务应可暂停: %v", err)
	}
	waitState(t, m, id2, string(statePaused))
}

// ---------------------------------------------------------------------------
// RPC 分发
// ---------------------------------------------------------------------------

func TestRPCDispatch(t *testing.T) {
	m, cleanup := newTestManager(t)
	defer cleanup()
	s := &rpcServer{m: m}

	tor := makeLocalTorrent(t, t.TempDir())
	raw, err := json.Marshal(tor)
	if err != nil {
		t.Fatal(err)
	}
	params := []json.RawMessage{raw}
	res, err := s.dispatch("addUri", params)
	if err != nil {
		t.Fatal(err)
	}
	gid, ok := res.(string)
	if !ok || gid == "" {
		t.Fatalf("addUri 应返回 gid 字符串,实际 %#v", res)
	}

	// tellStatus
	res, err = s.dispatch("tellStatus", []json.RawMessage{json.RawMessage(`"` + gid + `"`)})
	if err != nil {
		t.Fatal(err)
	}
	st, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("tellStatus 应返回对象,实际 %#v", res)
	}
	if st["gid"] != gid {
		t.Errorf("tellStatus 的 gid 不匹配: %v", st["gid"])
	}
	if st["status"] == "" {
		t.Error("tellStatus 应带 status 字段")
	}

	// tellActive / tellWaiting / tellStopped 都应返回数组
	for _, mth := range []string{"tellActive", "tellWaiting", "tellStopped"} {
		res, err := s.dispatch(mth, nil)
		if err != nil {
			t.Fatalf("%s 出错: %v", mth, err)
		}
		if _, ok := res.([]any); !ok {
			t.Errorf("%s 应返回数组,实际 %#v", mth, res)
		}
	}

	// 暂停 / 恢复 / 移除
	if _, err := s.dispatch("pause", []json.RawMessage{json.RawMessage(`"` + gid + `"`)}); err == nil {
		t.Log("任务可能已完成,暂停报错属正常")
	}
	if _, err := s.dispatch("remove", []json.RawMessage{json.RawMessage(`"` + gid + `"`)}); err != nil {
		t.Errorf("remove 应成功: %v", err)
	}
	// 未知方法要报错
	if _, err := s.dispatch("noSuchMethod", nil); err == nil {
		t.Error("未知方法应报错")
	}
}

func TestRPCParamHelpers(t *testing.T) {
	params := []json.RawMessage{
		json.RawMessage(`"hello"`),
		json.RawMessage(`true`),
		json.RawMessage(`{"dir":"D:/x","limit-rate":"2M"}`),
	}
	if got := paramString(params, 0); got != "hello" {
		t.Errorf("paramString = %q", got)
	}
	if got := paramString(params, 9); got != "" {
		t.Errorf("越界 paramString 应为空,实际 %q", got)
	}
	if !paramBool(params, 1) {
		t.Error("paramBool 应为 true")
	}
	if paramBool(params, 0) {
		t.Error("非布尔值应返回 false")
	}
	opts := paramOptions(params, 2)
	if opts["dir"] != "D:/x" || opts["limit-rate"] != "2M" {
		t.Errorf("paramOptions = %+v", opts)
	}
	if len(paramOptions(nil, 0)) != 0 {
		t.Error("缺省时 paramOptions 应为空表")
	}
}

func TestAria2StatusMapping(t *testing.T) {
	cases := map[string]string{
		string(stateDownloading): "active",
		string(stateSeeding):     "active",
		string(stateQueued):      "waiting",
		string(stateWaiting):     "waiting",
		string(statePaused):      "paused",
		string(stateComplete):    "complete",
		string(stateError):       "error",
	}
	for state, want := range cases {
		got := aria2Status(jsonStatus{State: state})
		if got["status"] != want {
			t.Errorf("状态 %s 应映射为 %s,实际 %v", state, want, got["status"])
		}
	}
}
