package main

// 分时段限速(带宽调度)。
//
// 场景:白天限速省带宽,夜间全速。在配置文件里写规则:
//
//	schedule.1=09:00-23:00,1M,256K   # 该时段 下载上限 1M,上传上限 256K
//	schedule.2=23:00-07:00,0,0       # 该时段不限速(0 = 不限)
//
// 每条规则:HH:MM-HH:MM,<下载上限>,<上传上限>,逗号分隔,支持跨零点(23:00-07:00)。
// 调度协程每 30 秒检查当前时间命中哪条规则,命中的话用 rule 的上限覆盖
// taskLimiter 的"基础全局上限";未命中任何规则时回退到无调度(各任务配额之和)。
//
// 实现要点:taskLimiter.setActive 每次按任务配额算出总闸;调度器再乘一个
// "当前时段的全局封顶"。封顶通过 scheduleCap 传给 setActive,为 0 表示本时段不限,
// 由任务配额之和决定总闸。

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// schedRule 是一条分时限速规则。
type schedRule struct {
	start, end time.Duration // 一天内的偏移(自 00:00)
	down, up   int64         // 0 表示该时段不限速
	overMid    bool          // 跨零点
	raw        string        // 原始配置行,便于报错
}

// schedSchedule 是一组规则。命中判断:遍历,第一条匹配即生效。
type schedSchedule struct {
	rules []schedRule
}

// parseScheduleFile 从配置文件解析 schedule.* 规则。cfg 的 key 形如 "schedule.1"。
func parseSchedule(cfg map[string]string) *schedSchedule {
	keys := make([]string, 0)
	for k := range cfg {
		if strings.HasPrefix(k, "schedule.") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // 按 schedule.1, schedule.2 ... 顺序匹配
	s := &schedSchedule{}
	for _, k := range keys {
		r, err := parseSchedRule(cfg[k])
		if err != nil {
			fmt.Fprintf(os.Stderr, "[调度] 忽略非法规则 %s=%s: %v\n", k, cfg[k], err)
			continue
		}
		s.rules = append(s.rules, r)
	}
	return s
}

// parseSchedRule 解析 "09:00-23:00,1M,256K"。
func parseSchedRule(raw string) (schedRule, error) {
	var r schedRule
	r.raw = raw
	parts := strings.Split(raw, ",")
	if len(parts) != 3 {
		return r, fmt.Errorf("格式应为 HH:MM-HH:MM,<下载>,<上传>")
	}
	se := strings.Split(strings.TrimSpace(parts[0]), "-")
	if len(se) != 2 {
		return r, fmt.Errorf("时间段应为 HH:MM-HH:MM")
	}
	st, err := parseHHMM(se[0])
	if err != nil {
		return r, err
	}
	en, err := parseHHMM(se[1])
	if err != nil {
		return r, err
	}
	r.start, r.end = st, en
	r.overMid = en < st
	if r.down, err = parseSize(strings.TrimSpace(parts[1])); err != nil {
		return r, fmt.Errorf("下载上限非法: %v", err)
	}
	if r.up, err = parseSize(strings.TrimSpace(parts[2])); err != nil {
		return r, fmt.Errorf("上传上限非法: %v", err)
	}
	return r, nil
}

// parseHHMM 解析 "09:30" 为一天内的时长偏移。
func parseHHMM(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	hm := strings.Split(s, ":")
	if len(hm) != 2 {
		return 0, fmt.Errorf("非法时间 %q,应为 HH:MM", s)
	}
	h, err1 := strconv.Atoi(hm[0])
	m, err2 := strconv.Atoi(hm[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("非法时间 %q", s)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

// match 判断给定时刻是否落在规则时间段内。跨零点分段判断。
func (r schedRule) match(now time.Duration) bool {
	if r.overMid {
		return now >= r.start || now < r.end
	}
	return now >= r.start && now < r.end
}

// current 返回 (now 时间偏移, 命中的规则, 是否命中)。
func (s *schedSchedule) current(t time.Time) (schedRule, bool) {
	if s == nil || len(s.rules) == 0 {
		return schedRule{}, false
	}
	off := time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second
	for _, r := range s.rules {
		if r.match(off) {
			return r, true
		}
	}
	return schedRule{}, false
}

// startSchedule 协程:每 30 秒重算当前时段封顶,写入 taskLimiter。
// 命中规则时,把 rule.down/up 作为全局封顶;未命中时封顶归零(由任务配额决定)。
func startSchedule(ctx context.Context, tl *taskLimiter, s *schedSchedule) {
	if s == nil || len(s.rules) == 0 {
		return
	}
	apply := func() {
		if _, hit := s.current(time.Now()); hit {
			r, _ := s.current(time.Now())
			tl.setScheduleCap(r.down, r.up)
			fmt.Fprintf(out, "[调度] 当前时段 %s: 下载上限 %s,上传上限 %s\n",
				r.raw, capOrUnlimited(r.down), capOrUnlimited(r.up))
		} else {
			tl.setScheduleCap(0, 0)
		}
	}
	apply() // 启动立即生效
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				apply()
			}
		}
	}()
}

func capOrUnlimited(n int64) string {
	if n <= 0 {
		return "不限"
	}
	return humanBps(n)
}

// ---------------------------------------------------------------------------
// 配置文件扩展:支持 [schedule] 块的解析已经在 loadConfigFile 中自动加载
// schedule.* 键。这里提供一个独立入口 --schedule=<文件>,便于把规则放单独文件。
// ---------------------------------------------------------------------------

func loadScheduleFile(path string) (*schedSchedule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg := map[string]string{}
	sc := bufio.NewScanner(f)
	i := 0
	for sc.Scan() {
		i++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		cfg[fmt.Sprintf("schedule.%d", i)] = line
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return parseSchedule(cfg), nil
}
