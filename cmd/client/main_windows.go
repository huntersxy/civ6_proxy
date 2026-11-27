//go:build windows

package main

import (
	"flag"
	"fmt"
	"runtime"
	"strings"
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
	elevated := flag.Bool("elevated", false, "内部标记：已尝试提权重启")
	flag.Parse()

	if *showVersion {
		messageBox("Civ6Relay", "Civ6RelayClient "+version, mbOK|mbIconInformation)
		return
	}
	if !win.IsAdmin() && !*preview {
		if !*elevated {
			args := buildRelaunchArgs(*diag, *noCapture)
			if err := win.RelaunchAsAdmin(args); err == nil {
				return
			} else {
				messageBox("需要管理员权限",
					fmt.Sprintf("需要管理员权限以加载 WinDivert 驱动：%v\n请右键「以管理员身份运行」。", err),
					mbOK|mbIconError)
				return
			}
		}
		messageBox("需要管理员权限",
			"未能获取管理员权限，请右键「以管理员身份运行」。",
			mbOK|mbIconError)
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
	s := newSession(!*noCapture, cfg.HostPCAP, cfg.HostP2P, cfg.MaxMembers)
	defer s.CloseLog()
	if err := runNativeUI(s, cfg); err != nil {
		s.logError("创建主窗口失败: %v", err)
		messageBox("启动失败", fmt.Sprintf("无法创建窗口: %v", err), mbOK|mbIconError)
	}
}

func buildRelaunchArgs(diag, noCapture bool) string {
	args := []string{"-elevated"}
	if diag {
		args = append(args, "-diag")
	}
	if noCapture {
		args = append(args, "-no-capture")
	}
	return strings.Join(args, " ")
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
