//go:build !windows

package main

// Unix-like 平台:用 x/term 把 stdin 切到原始模式(逐键读取)。
// 现代 terminal 原生支持 ANSI,无需像 Windows 那样额外开启 VT 处理。

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

type vtState struct {
	fd   int
	prev *term.State
	ok   bool
}

func enableVT() (*vtState, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, fmt.Errorf("stdin 不是终端(请在终端中运行 TUI)")
	}
	prev, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("进入原始模式失败: %w", err)
	}
	return &vtState{fd: fd, prev: prev, ok: true}, nil
}

func (st *vtState) restore() {
	if st == nil || !st.ok {
		return
	}
	_ = term.Restore(st.fd, st.prev)
	fmt.Print(ansiShowCursor)
}
