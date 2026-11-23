package main

// 历史记录数据库(SQLite,纯 Go 驱动 modernc.org/sqlite,无 CGO)。
//
// 记录每次"任务新完成"的事件:任务名、infohash、大小、下载/上传字节、耗时、
// 下载目录、完成时间、标签。用于:
//   - 去重提示:添加任务时若历史里已有同 infohash 的完成记录,提醒用户
//   - `history` 子命令查询最近记录 / 按 infohash 检索
//
// 数据库位置:~/.gotorrent/history.db(与 config/session 同目录),跨下载目录共享。

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动,注册 database/sql "sqlite" 名
)

// historyDB 包装 *sql.DB。
type historyDB struct {
	db *sql.DB
}

// historyPath 返回历史库路径。
func historyPath() string {
	if cfg := defaultConfigPath(); cfg != "" {
		return filepath.Join(filepath.Dir(cfg), "history.db")
	}
	return filepath.Join(mustAbs(opt.dir), ".gotorrent", "history.db")
}

// openHistory 打开(必要时创建)历史库并建表。失败返回 nil(历史非关键,降级)。
func openHistory() *historyDB {
	path := historyPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logWarn("创建历史目录失败: %v", err)
		return nil
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		logWarn("打开历史库失败: %v", err)
		return nil
	}
	// WAL 提升并发读写;busy_timeout 避免偶发锁冲突
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			logWarn("历史库设置 %s 失败: %v", pragma, err)
		}
	}
	h := &historyDB{db: db}
	if err := h.migrate(); err != nil {
		logWarn("初始化历史表失败: %v", err)
		db.Close()
		return nil
	}
	return h
}

func (h *historyDB) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS history (
	infohash   TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	downloaded INTEGER NOT NULL,
	uploaded   INTEGER NOT NULL,
	duration_s REAL NOT NULL,
	dir        TEXT NOT NULL,
	tag        TEXT,
	completed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_history_completed ON history(completed_at DESC);
`
	_, err := h.db.Exec(schema)
	return err
}

// Record 写入(或更新)一条完成记录。infohash 为主键,重复完成刷新统计。
func (h *historyDB) Record(r historyRecord) error {
	_, err := h.db.Exec(`
INSERT INTO history(infohash,name,size,downloaded,uploaded,duration_s,dir,tag,completed_at)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(infohash) DO UPDATE SET
	name=excluded.name, size=excluded.size,
	downloaded=excluded.downloaded, uploaded=excluded.uploaded,
	duration_s=excluded.duration_s, dir=excluded.dir,
	tag=excluded.tag, completed_at=excluded.completed_at`,
		r.InfoHash, r.Name, r.Size, r.Downloaded, r.Uploaded,
		r.DurationSec, r.Dir, r.Tag, r.CompletedAt.UTC().Format(time.RFC3339))
	return err
}

// Has 检查 infohash 是否已有完成记录(用于添加任务时去重提示)。
func (h *historyDB) Has(infoHash string) (historyRecord, bool) {
	var r historyRecord
	var completedAt string
	err := h.db.QueryRow(`SELECT infohash,name,size,completed_at FROM history WHERE infohash=?`,
		infoHash).Scan(&r.InfoHash, &r.Name, &r.Size, &completedAt)
	if err != nil {
		return r, false
	}
	if t, e := time.Parse(time.RFC3339, completedAt); e == nil {
		r.CompletedAt = t
	}
	return r, true
}

// Recent 返回最近 n 条记录。
func (h *historyDB) Recent(n int) ([]historyRecord, error) {
	rows, err := h.db.Query(`SELECT infohash,name,size,downloaded,uploaded,duration_s,dir,tag,completed_at
		FROM history ORDER BY completed_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []historyRecord
	for rows.Next() {
		var r historyRecord
		var completedAt string
		if err := rows.Scan(&r.InfoHash, &r.Name, &r.Size, &r.Downloaded, &r.Uploaded,
			&r.DurationSec, &r.Dir, &r.Tag, &completedAt); err != nil {
			return out, err
		}
		if t, e := time.Parse(time.RFC3339, completedAt); e == nil {
			r.CompletedAt = t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (h *historyDB) Close() error {
	if h == nil || h.db == nil {
		return nil
	}
	return h.db.Close()
}

// historyRecord 是一条完成记录。
type historyRecord struct {
	InfoHash    string
	Name        string
	Size        int64
	Downloaded  int64
	Uploaded    int64
	DurationSec float64
	Dir         string
	Tag         string
	CompletedAt time.Time
}

// ---------------------------------------------------------------------------
// history 子命令
// ---------------------------------------------------------------------------

func runHistory(args []string) {
	h := openHistory()
	if h == nil {
		fatalf("无法打开历史库: %s", historyPath())
	}
	defer h.Close()
	recs, err := h.Recent(50)
	if err != nil {
		fatalf("查询历史失败: %v", err)
	}
	if len(recs) == 0 {
		fmt.Println("(暂无历史记录)")
		return
	}
	fmt.Printf("%-12s %-34s %10s %s\n", "完成时间", "名称", "大小", "infohash")
	for _, r := range recs {
		name := trunc(r.Name, 34)
		fmt.Printf("%-12s %-34s %10s %s\n",
			r.CompletedAt.Local().Format("01-02 15:04"), name,
			human(r.Size), r.InfoHash)
	}
}
