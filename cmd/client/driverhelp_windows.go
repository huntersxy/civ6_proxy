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
	fmt.Fprintf(&b, "WinDivert 驱动文件不完整，无法抓包。\n")
	fmt.Fprintf(&b, "驱动目录: %s\n", st.Dir)
	fmt.Fprintf(&b, "DLL: %s\n", st.DLL.DescribeShort())
	fmt.Fprintf(&b, "SYS: %s\n", st.SYS.DescribeShort())
	av := win.SecuritySoftware()
	if len(av) > 0 {
		fmt.Fprintf(&b, "检测到安全软件: %s\n", strings.Join(av, "、"))
	}
	b.WriteString("判断: 文件在释放后缺失或哈希不一致，最常见原因是被杀毒软件隔离/删除。\n")
	b.WriteString(avHint(av))
	fmt.Fprintf(&b, "日志: %s\n", s.logLocation())
	return b.String()
}

func (s *session) driverFailureText(err error, dir string) string {
	st := win.InspectDriver(dir)
	svc := win.QueryWinDivertService()
	av := win.SecuritySoftware()
	var b strings.Builder
	fmt.Fprintf(&b, "启动 WinDivert 失败: %v\n", err)
	fmt.Fprintf(&b, "驱动目录: %s\n", dir)
	fmt.Fprintf(&b, "DLL: %s\n", st.DLL.DescribeShort())
	fmt.Fprintf(&b, "SYS: %s\n", st.SYS.DescribeShort())
	fmt.Fprintf(&b, "服务: %s\n", svc.Summary())
	fmt.Fprintf(&b, "当前管理员: %v\n", win.IsAdmin())
	if svc.External() {
		fmt.Fprintf(&b, "提示: WinDivert 服务由其他程序安装（ImagePath=%s），多个程序共用同名服务可能出现驱动版本冲突；如反复失败，先退出占用它的程序再重试。\n", svc.ImagePath)
	}
	if len(av) > 0 {
		fmt.Fprintf(&b, "检测到安全软件: %s\n", strings.Join(av, "、"))
	}
	code := uint32(0)
	var errno syscall.Errno
	if errors.As(err, &errno) {
		code = uint32(errno)
	}
	if code != 0 {
		fmt.Fprintf(&b, "Win32 错误码: %d\n", code)
	}
	b.WriteString(driverErrorHint(code, st, svc, av))
	fmt.Fprintf(&b, "日志: %s\n", s.logLocation())
	return b.String()
}

func driverErrorHint(code uint32, st win.DriverStatus, svc win.WinDivertService, av []string) string {
	var b strings.Builder
	switch code {
	case 2, 126:
		b.WriteString("判断: 找不到驱动文件/DLL。\n")
		if !st.SYS.Exists || !st.DLL.Exists {
			b.WriteString("驱动文件在释放后缺失，通常是杀毒软件隔离/删除。\n")
		} else if svc.Exists && !svc.ImageExists {
			b.WriteString("系统残留的 WinDivert 服务指向不存在的文件；管理员执行 sc.exe stop WinDivert 和 sc.exe delete WinDivert 后重试。\n")
		} else {
			b.WriteString("请检查杀软隔离区，或删除残留的 WinDivert 服务后重试。\n")
		}
	case 5:
		b.WriteString("判断: 拒绝访问。确认以管理员身份运行；已提权时通常是安全软件拦截。\n")
	case 193:
		b.WriteString("判断: DLL 损坏或架构不正确，请重新下载/解压客户端。\n")
	case 577:
		b.WriteString("判断: 驱动签名被系统拒绝，可能开启了内核隔离/易受攻击驱动黑名单，或企业策略限制。\n")
	case 1275:
		b.WriteString("判断: 驱动被系统策略阻止（易受攻击驱动黑名单）。\n")
	case 1058:
		b.WriteString("判断: WinDivert 服务被禁用；管理员执行 sc.exe config WinDivert start= demand 后重试。\n")
	default:
		b.WriteString("判断: 详见自动日志和诊断报告。\n")
	}
	b.WriteString(avHint(av))
	return b.String()
}

func avHint(av []string) string {
	joined := strings.Join(av, "、")
	switch {
	case strings.Contains(joined, "火绒"):
		return "处理: 火绒 → 防护中心 → 隔离区恢复 WinDivert64.dll/.sys；设置 → 信任区添加驱动目录和 Civ6RelayClient.exe；重启客户端。\n"
	case strings.Contains(joined, "360"):
		return "处理: 360 → 木马查杀 → 恢复区恢复文件；设置 → 信任区添加驱动目录和客户端 exe；重启客户端。\n"
	case strings.Contains(joined, "腾讯"):
		return "处理: 腾讯电脑管家 → 病毒查杀 → 隔离区恢复；设置 → 信任区添加驱动目录；重启客户端。\n"
	case strings.Contains(joined, "Defender"):
		return "处理: Windows 安全中心 → 病毒和威胁防护 → 保护历史记录恢复；管理设置 → 排除项添加驱动目录；重启客户端。\n"
	default:
		return "处理: 在杀软隔离区恢复 WinDivert64.dll/.sys，把驱动目录加入信任/排除项，然后重启客户端。\n"
	}
}
