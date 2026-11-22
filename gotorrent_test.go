package main

// 新增能力(配置 / 会话 / 批量输入 / glob 选择 / JSON / 每任务配额)的单元测试。
// 只覆盖纯逻辑,不依赖网络与真实 BT 客户端;端到端下载另用本地 tracker + seeder 冒烟。

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/time/rate"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"": 0, "0": 0, "512K": 512 << 10, "2M": 2 << 20, "1.5M": 1572864, "1G": 1 << 30}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseSize("abc"); err == nil {
		t.Error("parseSize 应拒绝非法值")
	}
}

func TestFileFilter(t *testing.T) {
	f := parseFilterSpec("*.mp4,!*sample*")
	if !f.set || !f.hasInclude() {
		t.Fatal("filter 解析失败")
	}
	if !f.match("Movie/main.mp4") {
		t.Error("*.mp4 应匹配任意层级下的 mp4")
	}
	if f.match("Movie/main.mkv") {
		t.Error("非 mp4 不应命中包含规则")
	}
	if !f.excluded("Movie/sample.mp4") {
		t.Error("! 规则应命中 sample")
	}
	if f.excluded("Movie/main.mp4") {
		t.Error("! 规则不应误伤正常文件")
	}
	onlyExclude := parseFilterSpec("!*.nfo")
	if onlyExclude.hasInclude() {
		t.Error("纯排除规则不应有包含规则")
	}
	if !onlyExclude.excluded("a.nfo") {
		t.Error("!*.nfo 应命中")
	}
}

func TestParseInputFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tasks.txt")
	body := "# 注释\n\n" +
		`magnet:?xt=urn:btih:aaaa&tr=udp%3A%2F%2F127.0.0.1%3A1%2Fannounce out=rename.mkv select-filter=*.mkv limit-rate=2M priority-high=*.mkv` + "\n" +
		`"C:\my videos\demo.torrent" --select=1,3 max-upload-limit=512K` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	specs, err := parseInputFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("应解析出 2 个任务,实际 %d", len(specs))
	}
	if specs[0].Out != "rename.mkv" || specs[0].SelectFilter != "*.mkv" || specs[0].LimitRate != "2M" {
		t.Errorf("行内参数解析错误: %+v", specs[0])
	}
	if specs[0].PriorityHigh != "*.mkv" {
		t.Errorf("priority-high 行内参数未解析: %+v", specs[0])
	}
	if specs[1].Input != `C:\my videos\demo.torrent` || specs[1].Select != "1,3" || specs[1].UploadRate != "512K" {
		t.Errorf("带空格路径/--前缀参数解析错误: %+v", specs[1])
	}
	bad := filepath.Join(dir, "bad.txt")
	_ = os.WriteFile(bad, []byte("magnet:?xt=urn:btih:x foo\n"), 0o644)
	if _, err := parseInputFile(bad); err == nil {
		t.Error("缺少 =value 应报错")
	}
}

func TestConfigLoaders(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	dir := filepath.Dir(defaultConfigPath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# 测试配置\n  --dir = \"D:/downloads\"  \nseed=false\nmax-conn=17\nseed-ratio=1.5\n; 忽略我\n"
	if err := os.WriteFile(defaultConfigPath(), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfigFile()
	if got := cfgStr(cfg, "dir", "."); got != "D:/downloads" {
		t.Errorf("cfgStr = %q", got)
	}
	if cfgBool(cfg, "seed", true) {
		t.Error("seed 应为 false")
	}
	if got := cfgInt(cfg, "max-conn", 50); got != 17 {
		t.Errorf("cfgInt = %d", got)
	}
	if got := cfgFloat(cfg, "seed-ratio", 0); got != 1.5 {
		t.Errorf("cfgFloat = %g", got)
	}
	if got := cfgBool(cfg, "not-exist", true); !got {
		t.Error("缺失键应回退默认值")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	old := opt
	defer func() { opt = old }()
	opt.dir = filepath.Join(t.TempDir(), "dl")
	opt.seed = false
	opt.maxConcurrent = 2
	specs := []taskSpec{{Input: "magnet:?xt=urn:btih:deadbeef", SelectFilter: "*.mp4", LimitRate: "1M"}}
	if err := saveSession(specs); err != nil {
		t.Fatal(err)
	}
	s, err := loadSession()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tasks) != 1 || s.Tasks[0].SelectFilter != "*.mp4" || s.Tasks[0].LimitRate != "1M" {
		t.Fatalf("会话任务丢失: %+v", s.Tasks)
	}
	if !filepath.IsAbs(s.Options.Dir) || s.Options.MaxConcurrent != 2 || s.Options.Seed {
		t.Errorf("会话选项丢失: %+v", s.Options)
	}
	if err := saveSession(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionPath()); !os.IsNotExist(err) {
		t.Error("空会话应删除文件")
	}
}

func TestTaskLimiterQuota(t *testing.T) {
	old := opt
	defer func() { opt = old }()
	opt.limitRate, opt.maxUploadRate = "50K", "50K" // 全局缺省
	tl := newTaskLimiter()
	tasks := []*task{
		{spec: taskSpec{LimitRate: "1M"}},
		{spec: taskSpec{LimitRate: "2M", UploadRate: "512K"}},
		{spec: taskSpec{}, done: true}, // 已完成任务不占配额
	}
	tl.setActive(tasks)
	if got := tl.down.Limit(); got != rate.Limit(3<<20) {
		t.Errorf("总下载闸应为 1M+2M=3M,实际 %g", float64(got))
	}
	if got := tl.up.Limit(); got != rate.Limit(50<<10+512<<10) {
		t.Errorf("总上传闸应为 (全局50K)+512K,实际 %g", float64(got))
	}
	// 完成一个任务后总闸回落
	tasks[1].done = true
	tl.setActive(tasks)
	if got := tl.down.Limit(); got != rate.Limit(1<<20) {
		t.Errorf("完成补位后总闸应回落到 1M,实际 %g", float64(got))
	}
	opt.limitRate, opt.maxUploadRate = "", ""
	tl.setActive(nil)
	if tl.down.Limit() != rate.Inf || tl.up.Limit() != rate.Inf {
		t.Error("无配额时应为 rate.Inf(不限速),不能是 0")
	}
}

func TestLooksNumericSpec(t *testing.T) {
	cases := map[string]bool{
		"":        true, // 空串按编号处理,数字解析为空集合
		"1":       true,
		"1,3-5":   true,
		" 1 , 2 ": true,
		"*.mp4":   false,
		"1,*.mkv": false,
		"!*.nfo":  false,
		"Movie/*": false,
	}
	for in, want := range cases {
		if got := looksNumericSpec(in); got != want {
			t.Errorf("looksNumericSpec(%q) = %v; want %v", in, got, want)
		}
	}
}

func TestSessionAdvancedOptions(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	old := opt
	defer func() { opt = old }()
	opt.dir = filepath.Join(t.TempDir(), "dl")
	opt.encryption = "required"
	opt.trackersCustom = []string{"udp://a.example:1/announce", "udp://b.example:2/announce"}
	opt.priorityHigh = "*.mp4"
	specs := []taskSpec{{Input: "x.torrent", PriorityHigh: "1,3"}}
	if err := saveSession(specs); err != nil {
		t.Fatal(err)
	}
	s, err := loadSession()
	if err != nil {
		t.Fatal(err)
	}
	if s.Options.Encryption != "required" {
		t.Errorf("encryption 未保存: %q", s.Options.Encryption)
	}
	if len(s.Options.Trackers) != 2 || s.Options.Trackers[1] != "udp://b.example:2/announce" {
		t.Errorf("trackers 未保存: %+v", s.Options.Trackers)
	}
	if s.Options.PriorityHigh != "*.mp4" {
		t.Errorf("全局 priority_high 未保存: %q", s.Options.PriorityHigh)
	}
	if len(s.Tasks) != 1 || s.Tasks[0].PriorityHigh != "1,3" {
		t.Errorf("任务级 priority_high 未保存: %+v", s.Tasks)
	}
	_ = saveSession(nil)
}
