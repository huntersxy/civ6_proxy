//go:build windows

package main

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"civ6relay/internal/win"
)

func (s *session) driverIncompleteText(st win.DriverStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "WinDivert 驱动文件不完整 (%s)\n", st.Dir)
	fmt.Fprintf(&b, "DLL: %s | SYS: %s\n", st.DLL.DescribeShort(), st.SYS.DescribeShort())
	av := win.SecuritySoftware()
	if len(av) > 0 {
		fmt.Fprintf(&b, "安全软件: %s\n", strings.Join(av, "、"))
	}
	b.WriteString(avHint(av))
	return b.String()
}

func (s *session) driverFailureText(err error, dir string) string {
	st := win.InspectDriver(dir)
	svc := win.QueryWinDivertService()
	av := win.SecuritySoftware()
	var b strings.Builder
	fmt.Fprintf(&b, "启动 WinDivert 失败: %v\n", err)
	fmt.Fprintf(&b, "目录: %s | DLL: %s | SYS: %s\n", dir, st.DLL.DescribeShort(), st.SYS.DescribeShort())
	fmt.Fprintf(&b, "服务: %s | 管理员: %v\n", svc.Summary(), win.IsAdmin())
	if svc.External() {
		fmt.Fprintf(&b, "外部 WinDivert 服务冲突 (%s)\n", svc.ImagePath)
	}
	if len(av) > 0 {
		fmt.Fprintf(&b, "安全软件: %s\n", strings.Join(av, "、"))
	}
	code := uint32(0)
	var errno syscall.Errno
	if errors.As(err, &errno) {
		code = uint32(errno)
	}
	if code != 0 {
		fmt.Fprintf(&b, "错误码: %d\n", code)
	}
	b.WriteString(driverErrorHint(code, st, svc, av))
	return b.String()
}

func driverErrorHint(code uint32, st win.DriverStatus, svc win.WinDivertService, av []string) string {
	var b strings.Builder
	switch code {
	case 2, 126:
		if !st.SYS.Exists || !st.DLL.Exists {
			b.WriteString("驱动文件缺失，可能被杀软隔离。\n")
		} else if svc.Exists && !svc.ImageExists {
			b.WriteString("残留 WinDivert 服务无效，请执行 sc.exe stop WinDivert && sc.exe delete WinDivert。\n")
		} else {
			b.WriteString("找不到驱动文件，请检查杀软隔离区或清理 WinDivert 服务。\n")
		}
	case 5:
		b.WriteString("拒绝访问，请确认管理员权限或安全软件拦截。\n")
	case 193:
		b.WriteString("DLL 损坏或架构不匹配。\n")
	case 577:
		b.WriteString("驱动签名被拒绝（内核隔离或策略限制）。\n")
	case 1275:
		b.WriteString("驱动被系统黑名单阻止。\n")
	case 1058:
		b.WriteString("WinDivert 服务被禁用，请执行 sc.exe config WinDivert start= demand。\n")
	}
	b.WriteString(avHint(av))
	return b.String()
}

func avHint(av []string) string {
	joined := strings.Join(av, "、")
	switch {
	case strings.Contains(joined, "火绒"):
		return "火绒：隔离区恢复 WinDivert64.dll/.sys 并加入信任区。\n"
	case strings.Contains(joined, "360"):
		return "360：恢复区还原文件并加入信任区。\n"
	case strings.Contains(joined, "腾讯"):
		return "电脑管家：隔离区恢复并加入信任区。\n"
	case strings.Contains(joined, "Defender"):
		return "Defender：保护历史记录恢复并添加驱动目录排除项。\n"
	default:
		return "请在杀软隔离区恢复 WinDivert64.dll/.sys 并将驱动目录加入信任区。\n"
	}
}
