//go:build windows

package main

import (
	"flag"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"civ6relay/internal/win"
)

var version = "dev"

func main() {
	// The Win32 UI needs one stable OS thread for its message loop.
	runtime.LockOSThread()

	diag := flag.Bool("diag", false, "运行驱动/网络诊断后退出")
	showVersion := flag.Bool("version", false, "显示版本后退出")
	preview := flag.Bool("preview", false, "跳过提权，仅预览界面（调试用）")
	noCapture := flag.Bool("no-capture", false, "跳过 WinDivert 抓包（调试用）")
	flag.Parse()

	if *showVersion {
		messageBox("Civ6Relay", "Civ6RelayClient "+version, mbOK|mbIconInformation)
		return
	}
	if !win.IsAdmin() && !*preview {
		answer := messageBoxYesNo("需要管理员权限",
			"Civ6Relay 需要管理员权限来加载 WinDivert 驱动并配置游戏入站防火墙规则。\n\n现在以管理员身份重新启动吗？")
		if answer != 6 { // IDYES
			return
		}
		args := ""
		if *diag {
			args = "-diag"
		}
		if err := win.RelaunchAsAdmin(args); err != nil {
			messageBox("提权失败", err.Error(), mbOK|mbIconError)
		}
		return
	}
	if *diag {
		runDiagStandalone()
		return
	}
	if !acquireSingleInstance() {
		focusExistingWindow()
		return
	}

	cfg := loadConfig()
	s := newSession(!*noCapture)
	s.setConfig(cfg)
	s.logEnvironment()
	defer s.CloseLog()
	if err := runNativeUI(s, cfg); err != nil {
		s.logError("创建主窗口失败: %v", err)
		messageBox("启动失败", fmt.Sprintf("无法创建窗口: %v", err), mbOK|mbIconError)
	}
	s.logInfo("程序退出")
}

func messageBox(title, text string, flags uint32) {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	textPtr, _ := syscall.UTF16PtrFromString(text)
	_, _, _ = proc.Call(0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)
}

func messageBoxYesNo(title, text string) int {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	textPtr, _ := syscall.UTF16PtrFromString(text)
	r, _, _ := proc.Call(0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(mbYesNo|mbIconQuestion),
	)
	return int(r)
}
