//go:build windows

package main

// Windows 控制台:开启 VT 处理让 ANSI 生效,并把输入切到逐键原始模式。

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// vtState 保存进入/退出原始模式的终端状态。
type vtState struct {
	inMode  uint32
	outMode uint32
	inH     windows.Handle
	outH    windows.Handle
	ok      bool
}

// enableVT 让控制台识别 ANSI 转义并关闭行输入/回显(原始输入)。
func enableVT() (*vtState, error) {
	inH := windows.Handle(os.Stdin.Fd())
	outH := windows.Handle(os.Stdout.Fd())
	st := &vtState{inH: inH, outH: outH}
	var inMode, outMode uint32
	if err := windows.GetConsoleMode(inH, &inMode); err != nil {
		return nil, fmt.Errorf("stdin 不是控制台(请在终端中运行 TUI): %w", err)
	}
	if err := windows.GetConsoleMode(outH, &outMode); err != nil {
		return nil, fmt.Errorf("stdout 不是控制台: %w", err)
	}
	st.inMode, st.outMode = inMode, outMode
	if err := windows.SetConsoleMode(outH, outMode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return nil, fmt.Errorf("终端不支持 VT 处理(需 Windows 10+): %w", err)
	}
	const wantIn = windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	newIn := (inMode | wantIn) &^ (windows.ENABLE_LINE_INPUT | windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT)
	if err := windows.SetConsoleMode(inH, newIn); err != nil {
		// 输出 VT 已开,输入模式失败仍可退回逐行(体验降级但不致命)。
		st.ok = true
		return st, nil
	}
	st.ok = true
	return st, nil
}

// restore 恢复终端到进入前的模式并重新显示光标。
func (st *vtState) restore() {
	if st == nil || !st.ok {
		return
	}
	_ = windows.SetConsoleMode(st.inH, st.inMode)
	_ = windows.SetConsoleMode(st.outH, st.outMode)
	fmt.Print(ansiShowCursor)
}
