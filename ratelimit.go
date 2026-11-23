package main

// 每任务独立限速。
//
// anacrolix/torrent v1.61 只提供 Client 级(全局)的 DownloadRateLimiter /
// UploadRateLimiter(*rate.Limiter,具体类型,非接口),无法按 torrent 注入独立
// limiter;改多 Client 会引入多端口、多 DHT、多个 bbolt 断点库,回归风险太高。
//
// 折中且有效的做法:每个任务声明自己的配额,全局 limiter 的速率随"当前活跃
// 任务的配额之和"动态调整(rate.Limiter.SetLimit 并发安全)。这样 N 个任务各
// 配 1M 时总闸是 N M,不会挤在 1M 上互相抢;某任务完成后总闸自动回落。
// 注意:limit=0 会被 Client.checkConfig() 判为错误配置,不限速必须用 rate.Inf。

import (
	"sync"

	"golang.org/x/time/rate"
)

const limiterBurst = 1 << 20

type taskLimiter struct {
	mu     sync.Mutex
	down   *rate.Limiter
	up     *rate.Limiter
	active map[*task]struct{}
	// scheduleCap 是分时段限速器设置的"全局封顶"(0 = 本时段不限,由任务配额决定)。
	capDown, capUp int64
}

func newTaskLimiter() *taskLimiter {
	return &taskLimiter{
		down:   rate.NewLimiter(rate.Inf, limiterBurst),
		up:     rate.NewLimiter(rate.Inf, limiterBurst),
		active: map[*task]struct{}{},
	}
}

// setActive 根据当前活跃任务重算总闸速率。每个任务的配额来自
// 该任务 spec.LimitRate / spec.UploadRate,为空时回退到全局 opt。
func (l *taskLimiter) setActive(tasks []*task) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active = map[*task]struct{}{}
	var down, up int64
	for _, tk := range tasks {
		if tk.done {
			continue
		}
		l.active[tk] = struct{}{}
		d, err := parseSize(firstNonEmpty(tk.spec.LimitRate, opt.limitRate))
		if err == nil && d > 0 {
			down += d
		}
		u, err := parseSize(firstNonEmpty(tk.spec.UploadRate, opt.maxUploadRate))
		if err == nil && u > 0 {
			up += u
		}
	}
	setRate(l.down, minCap(down, l.capDown))
	setRate(l.up, minCap(up, l.capUp))
}

// setScheduleCap 由调度协程调用,设置当前时段的全局封顶后立即重算总闸。
// 0 表示本时段不限(总闸由任务配额之和决定)。
func (l *taskLimiter) setScheduleCap(down, up int64) {
	l.mu.Lock()
	l.capDown, l.capUp = down, up
	downSum, upSum := l.sumLocked()
	setRate(l.down, minCap(downSum, down))
	setRate(l.up, minCap(upSum, up))
	l.mu.Unlock()
}

func (l *taskLimiter) sumLocked() (down, up int64) {
	for tk := range l.active {
		if tk.done {
			continue
		}
		if d, err := parseSize(firstNonEmpty(tk.spec.LimitRate, opt.limitRate)); err == nil && d > 0 {
			down += d
		}
		if u, err := parseSize(firstNonEmpty(tk.spec.UploadRate, opt.maxUploadRate)); err == nil && u > 0 {
			up += u
		}
	}
	return down, up
}

func minCap(sum, capVal int64) int64 {
	if capVal > 0 && (sum <= 0 || sum > capVal) {
		return capVal
	}
	return sum
}

func setRate(l *rate.Limiter, total int64) {
	if total <= 0 {
		l.SetLimit(rate.Inf)
		l.SetBurst(limiterBurst)
		return
	}
	burst := int64(limiterBurst)
	if total > burst {
		burst = total
	}
	l.SetBurst(int(burst))
	l.SetLimit(rate.Limit(total))
}

func (l *taskLimiter) downloadLimiter() *rate.Limiter { return l.down }
func (l *taskLimiter) uploadLimiter() *rate.Limiter   { return l.up }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
