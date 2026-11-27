//go:build windows

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"civ6relay/internal/win"
)

// collectDiagnostics gathers driver, security software, service, Defender,
// network and firewall facts, writes a report to %TEMP%\Civ6RelayDiag.txt and
// returns a short summary.
func collectDiagnostics(relayText, logPath string) (summary, report, reportPath string, ok bool) {
	ok = true
	var b strings.Builder
	var status []string
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}
	fail := func(format string, args ...any) {
		ok = false
		line("失败: "+format, args...)
	}

	admin := win.IsAdmin()
	line("Civ6Relay 诊断报告")
	line("时间: %s | 版本: %s (%s/%s)", time.Now().Format("2006-01-02 15:04:05"), version, runtime.GOOS, runtime.GOARCH)
	line("系统: %s | 管理员: %v", win.OSVersion(), admin)
	if logPath == "" {
		logPath = "%LOCALAPPDATA%\\Civ6Relay\\logs"
	}
	line("日志: %s", logPath)
	status = append(status, fmt.Sprintf("系统: %s | 管理员: %v", win.OSVersion(), admin))

	line("")
	line("== 安全软件 ==")
	av := win.SecuritySoftware()
	if len(av) == 0 {
		line("未检测到")
		status = append(status, "安全软件: 未检测到")
	} else {
		line("检测到: %s", strings.Join(av, "、"))
		status = append(status, "安全软件: "+strings.Join(av, "、"))
	}

	line("")
	line("== 驱动文件 ==")
	driverDir, err := win.EnsureDriver()
	var files win.DriverStatus
	if err != nil {
		fail("释放驱动: %v", err)
		files = win.InspectDriver(win.DriverDir())
		status = append(status, "驱动文件: 释放失败")
	} else {
		files = win.InspectDriver(driverDir)
		line("目录: %s", files.Dir)
		line("WinDivert64.dll: %s", files.DLL.Describe())
		line("WinDivert64.sys: %s", files.SYS.Describe())
		if files.Complete {
			status = append(status, "驱动文件: 完整")
		} else {
			ok = false
			status = append(status, "驱动文件: 缺失或校验失败")
			line(avHint(av))
		}
	}

	line("")
	line("== WinDivert 服务 ==")
	svc := win.QueryWinDivertService()
	line("服务: %s", svc.Summary())
	if svc.Exists {
		if !svc.ImageExists {
			line("ImagePath 不存在，建议清理残留服务: sc.exe stop WinDivert && sc.exe delete WinDivert")
		}
		if svc.External() {
			line("外部 WinDivert 服务 (%s)，可能冲突", svc.ImagePath)
		}
	}
	status = append(status, "服务: "+svc.Summary())

	line("")
	line("== 驱动加载 ==")
	if driverDir != "" {
		if _, err := win.LoadDriver(driverDir); err != nil {
			fail("LoadLibrary: %v", err)
			status = append(status, "驱动加载: 失败")
			line(driverErrorHint(errnoOf(err), files, svc, av))
		} else {
			line("LoadLibrary: 成功")
			if err := win.TestDriver(driverDir); err != nil {
				fail("WinDivertOpen: %v", err)
				status = append(status, "驱动加载: 失败")
				line(driverErrorHint(errnoOf(err), files, svc, av))
			} else {
				line("WinDivertOpen: 成功")
				status = append(status, "驱动加载: 成功")
			}
		}
	}

	line("")
	line("== Defender ==")
	def := win.QueryDefender()
	if def.Error != "" {
		line("查询失败: %s", def.Error)
		status = append(status, "Defender: 查询失败")
	} else {
		matches := 0
		for _, item := range def.Quarantine {
			lower := strings.ToLower(item)
			if strings.Contains(lower, "windivert") || strings.Contains(lower, "civ6relay") {
				matches++
				if matches <= 5 {
					line("隔离: %s", item)
				}
			}
		}
		excluded := false
		for _, item := range def.Exclusions {
			lower := strings.ToLower(item)
			if strings.Contains(lower, "civ6relay") || strings.Contains(lower, "windivert") {
				excluded = true
			}
		}
		line("隔离记录: %d | 排除项: %v", matches, excluded)
		status = append(status, fmt.Sprintf("Defender: 隔离 %d, 排除=%v", matches, excluded))
	}

	line("")
	line("== 网络 ==")
	host, port, err := parseRelayAddress(relayText)
	if err != nil {
		line("中转地址: 未配置")
		status = append(status, "中转: 未配置")
	} else {
		line("中转: %s", win.FormatRelay(host, port))
		ips, err := net.LookupIP(host)
		var ipv4 net.IP
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				ipv4 = v4
				break
			}
		}
		if err != nil || ipv4 == nil {
			fail("DNS 解析失败: %v", err)
			status = append(status, "中转解析: 失败")
		} else {
			line("解析: %s", ipv4)
			if route, err := win.RouteTo(ipv4); err != nil {
				fail("查询出口失败: %v", err)
				status = append(status, "出口: 失败")
			} else {
				line("出口: %s (if=%d)", route.LocalIP, route.IfIndex)
				status = append(status, "出口: "+route.LocalIP.String())
			}
		}
	}
	if subnet, err := win.SelectVirtualSubnet(); err != nil {
		fail("虚拟网段: %v", err)
		status = append(status, "虚拟网段: 失败")
	} else {
		line("虚拟网段: %s", subnet)
		status = append(status, "虚拟网段: "+subnet.String())
	}

	line("")
	line("== 防火墙 ==")
	if win.FirewallRuleExists() {
		line("Civ6Relay-Game-Inbound: 已存在")
		status = append(status, "防火墙: 已存在")
	} else {
		line("Civ6Relay-Game-Inbound: 未创建")
		status = append(status, "防火墙: 未创建")
	}

	reportPath = filepath.Join(os.TempDir(), "Civ6RelayDiag.txt")
	content := append([]byte{0xEF, 0xBB, 0xBF}, []byte(b.String())...)
	_ = os.WriteFile(reportPath, content, 0o644)
	summary = strings.Join(status, "\n")
	if ok {
		summary += "\n\n结果: 通过"
	} else {
		summary += "\n\n结果: 发现问题"
	}
	return summary, b.String(), reportPath, ok
}

func errnoOf(err error) uint32 {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return uint32(errno)
	}
	return 0
}

func runDiagStandalone() {
	cfg := loadConfig()
	summary, _, path, ok := collectDiagnostics(win.FormatRelay(cfg.RelayHost, cfg.RelayPort), "")
	icon := uint32(mbIconInformation)
	if !ok {
		icon = mbIconError
	}
	messageBox("Civ6Relay 诊断", summary+"\n\n完整报告已保存:\n"+path, icon)
}
