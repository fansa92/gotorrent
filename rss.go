package main

// RSS 订阅:定期抓取订阅源,按正则过滤标题,命中后自动加入下载队列。
//
// 配置文件(--rss-file)用分块格式,风格与 ~/.gotorrent/config 一致:
//
//	# 注释
//	url=https://example.com/feed.xml
//	include=.*1080p.*
//	exclude=.*720p.*
//	dir=D:/media/movies
//	select-filter=*.mkv
//	limit-rate=2M
//
// 空行 / 注释分隔订阅块,每个块以 url= 开始,后面的键值属于该订阅。
//
// 支持 RSS 2.0 与 Atom 两种格式;种子地址的取用顺序:
//  1. rel=enclosure 的链接(或以 .torrent 结尾的链接)
//  2. 磁力链接
//  3. 普通链接(当它是磁力链接时)
//
// 去重:抓到的条目按"订阅源 + 条目 GUID"记在 <配置文件>.seen 里,重启后
// 不会重复下载已经处理过的条目。

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const rssUserAgent = "gotorrent/1.0 (+RSS subscription)"

// rssSubscription 是一个订阅源及其过滤规则。
type rssSubscription struct {
	URL          string
	Include      []*regexp.Regexp
	Exclude      []*regexp.Regexp
	Dir          string
	SelectFilter string
	LimitRate    string
	Out          string
}

// match 判断条目标题是否命中规则。
// include 为空表示全要;exclude 命中一律排除(排除优先)。
func (s rssSubscription) match(title string) bool {
	for _, re := range s.Exclude {
		if re.MatchString(title) {
			return false
		}
	}
	if len(s.Include) == 0 {
		return true
	}
	for _, re := range s.Include {
		if re.MatchString(title) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 配置解析
// ---------------------------------------------------------------------------

// parseRSSFile 解析订阅配置文件。
func parseRSSFile(path string) ([]rssSubscription, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var subs []rssSubscription
	var cur *rssSubscription
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d 无法解析的行: %s", path, lineNo, line)
		}
		k = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(k), "--"))
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k == "url" {
			subs = append(subs, rssSubscription{URL: v})
			cur = &subs[len(subs)-1]
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("%s:%d 参数 %q 必须放在 url= 之后", path, lineNo, k)
		}
		switch k {
		case "include":
			re, err := regexp.Compile(v)
			if err != nil {
				return nil, fmt.Errorf("%s:%d include 正则非法 %q: %w", path, lineNo, v, err)
			}
			cur.Include = append(cur.Include, re)
		case "exclude":
			re, err := regexp.Compile(v)
			if err != nil {
				return nil, fmt.Errorf("%s:%d exclude 正则非法 %q: %w", path, lineNo, v, err)
			}
			cur.Exclude = append(cur.Exclude, re)
		case "dir":
			cur.Dir = v
		case "select-filter":
			cur.SelectFilter = v
		case "limit-rate":
			cur.LimitRate = v
		case "out":
			cur.Out = v
		default:
			return nil, fmt.Errorf("%s:%d 未知参数 %q", path, lineNo, k)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, fmt.Errorf("%s: 没有配置任何订阅源", path)
	}
	return subs, nil
}

// ---------------------------------------------------------------------------
// Feed 解析
// ---------------------------------------------------------------------------

// feedLink 是一个候选下载地址。
type feedLink struct {
	Href string
	Rel  string
	Type string
}

// feedItem 是统一后的条目(RSS 与 Atom 归一化到这里)。
type feedItem struct {
	Title string
	GUID  string
	Date  time.Time
	Links []feedLink
}

// feedLinkXML 同时兼容 Atom(<link href=...>)与 RSS 之外的属性写法。
type feedLinkXML struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

// rssItem 是 RSS 2.0 的一个 item。
type rssItem struct {
	Title   string `xml:"title"`
	Link    string `xml:"link"`
	GUID    string `xml:"guid"`
	PubDate string `xml:"pubDate"`
	Enclos  struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	} `xml:"enclosure"`
}

// rssFeed 是 RSS 2.0 文档。
type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

// atomEntry 是 Atom 的一个 entry。
type atomEntry struct {
	Title   string        `xml:"title"`
	ID      string        `xml:"id"`
	Updated string        `xml:"updated"`
	Links   []feedLinkXML `xml:"link"`
}

// atomFeed 是 Atom 文档。
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

// parseFeed 解析 RSS 2.0 或 Atom,返回归一化后的条目列表。
func parseFeed(data []byte) ([]feedItem, error) {
	var probe struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("解析订阅源失败: %w", err)
	}
	switch strings.ToLower(probe.XMLName.Local) {
	case "rss":
		var f rssFeed
		if err := xml.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("解析 RSS 失败: %w", err)
		}
		out := make([]feedItem, 0, len(f.Channel.Items))
		for _, it := range f.Channel.Items {
			fi := feedItem{Title: strings.TrimSpace(it.Title), GUID: strings.TrimSpace(it.GUID)}
			if fi.GUID == "" {
				fi.GUID = it.Link
			}
			fi.Date = parseFeedTime(it.PubDate)
			if it.Link != "" {
				fi.Links = append(fi.Links, feedLink{Href: it.Link})
			}
			if it.Enclos.URL != "" {
				fi.Links = append(fi.Links, feedLink{Href: it.Enclos.URL, Rel: "enclosure", Type: it.Enclos.Type})
			}
			out = append(out, fi)
		}
		return out, nil
	case "feed":
		var f atomFeed
		if err := xml.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("解析 Atom 失败: %w", err)
		}
		out := make([]feedItem, 0, len(f.Entries))
		for _, e := range f.Entries {
			fi := feedItem{Title: strings.TrimSpace(e.Title), GUID: strings.TrimSpace(e.ID)}
			fi.Date = parseFeedTime(e.Updated)
			for _, l := range e.Links {
				fi.Links = append(fi.Links, feedLink{Href: l.Href, Rel: l.Rel, Type: l.Type})
			}
			out = append(out, fi)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("不支持的订阅源格式: <%s>", probe.XMLName.Local)
	}
}

// parseFeedTime 尽力解析几种常见的时间格式,失败返回零值。
func parseFeedTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC1123Z, time.RFC1123, time.RFC3339,
		time.RFC822Z, time.RFC822, "Mon, 02 Jan 2006 15:04:05 -0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// 去重状态
// ---------------------------------------------------------------------------

// rssState 记录已经处理过的条目。
type rssState struct {
	path string
	seen map[string]bool
}

// loadRSSState 读取去重状态文件;不存在时返回空状态。
func loadRSSState(configPath string) (*rssState, error) {
	p := configPath + ".seen"
	st := &rssState{path: p, seen: map[string]bool{}}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			st.seen[line] = true
		}
	}
	return st, sc.Err()
}

// save 原子写入去重状态文件。
func (st *rssState) save() error {
	if st == nil || len(st.seen) == 0 {
		return nil
	}
	dir := filepath.Dir(st.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".rss-seen-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	w := bufio.NewWriter(f)
	for k := range st.seen {
		if _, err := w.WriteString(k + "\n"); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, st.path)
}

// ---------------------------------------------------------------------------
// 轮询
// ---------------------------------------------------------------------------

// rssPoller 负责定期抓取订阅源并把命中项加入下载队列。
type rssPoller struct {
	subs  []rssSubscription
	state *rssState
	m     *Manager
}

// startRSS 启动订阅轮询。interval 为分钟数,<=0 表示不轮询(只解析配置)。
func startRSS(ctx context.Context, m *Manager, path string, interval int) error {
	subs, err := parseRSSFile(path)
	if err != nil {
		return err
	}
	st, err := loadRSSState(path)
	if err != nil {
		return err
	}
	p := &rssPoller{subs: subs, state: st, m: m}
	fmt.Fprintf(out, "[RSS] 已加载 %d 个订阅源\n", len(subs))
	if interval > 0 {
		go p.loop(ctx, time.Duration(interval)*time.Minute)
	}
	return nil
}

// loop 定期轮询。启动时先抓一次,避免刚启动要等一个周期才有反应。
func (p *rssPoller) loop(ctx context.Context, interval time.Duration) {
	p.poll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

// poll 抓取所有订阅源,处理新条目。
func (p *rssPoller) poll(ctx context.Context) {
	changed := false
	for _, sub := range p.subs {
		items, err := fetchFeed(ctx, sub.URL)
		if err != nil {
			logWarn("%s 抓取失败: %v", sub.URL, err)
			continue
		}
		for _, it := range items {
			if !sub.match(it.Title) {
				continue
			}
			key := sub.URL + "\x00" + it.GUID
			if p.state.seen[key] {
				continue
			}
			// 先记已处理,再下载:即使下载失败也不会卡在同一条上无限重试。
			p.state.seen[key] = true
			changed = true
			input, err := resolveTorrent(ctx, it)
			if err != nil {
				logWarn("%s 无法解析种子地址: %v", it.Title, err)
				continue
			}
			spec := taskSpec{
				Input:        input,
				Out:          sub.Out,
				SelectFilter: sub.SelectFilter,
				LimitRate:    sub.LimitRate,
			}
			if id, err := p.m.Add(spec); err != nil {
				logWarn("%s 加入队列失败: %v", it.Title, err)
			} else {
				fmt.Fprintf(out, "[RSS] 已加入: %s (%s)\n", it.Title, id[:16])
			}
		}
	}
	if changed {
		if err := p.state.save(); err != nil {
			logWarn("保存去重状态失败: %v", err)
		}
	}
}

// fetchFeed 抓取并解析一个订阅源。
func fetchFeed(ctx context.Context, url string) ([]feedItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", rssUserAgent)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	return parseFeed(body)
}

// resolveTorrent 从条目里找出种子地址;必要时把 .torrent 下载到本地。
func resolveTorrent(ctx context.Context, it feedItem) (string, error) {
	// 1) 优先 enclosure 或 .torrent 结尾的链接
	for _, l := range it.Links {
		low := strings.ToLower(l.Href)
		if l.Rel == "enclosure" || strings.HasSuffix(low, ".torrent") {
			if isMagnet(l.Href) {
				return l.Href, nil
			}
			return downloadTorrent(ctx, l.Href)
		}
	}
	// 2) 退而求其次:任意磁力链接
	for _, l := range it.Links {
		if isMagnet(l.Href) {
			return l.Href, nil
		}
	}
	return "", fmt.Errorf("条目里没有找到种子或磁力链接")
}

// downloadTorrent 下载 .torrent 到临时文件,返回本地路径。
func downloadTorrent(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", rssUserAgent)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载种子失败: HTTP %d", resp.StatusCode)
	}
	dir := filepath.Join(mustAbs(opt.dir), ".gotorrent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "rss-*.torrent")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 64<<20)); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("写入种子文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("关闭种子文件失败: %w", err)
	}
	return name, nil
}
