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

	line("Civ6Relay 诊断报告")
	line("时间: %s", time.Now().Format("2006-01-02 15:04:05"))
	line("版本: %s", version)
	line("平台: %s/%s", runtime.GOOS, runtime.GOARCH)
	line("系统: %s", win.OSVersion())
	admin := win.IsAdmin()
	line("管理员权限: %v", admin)
	if logPath == "" {
		logPath = "%LOCALAPPDATA%\\Civ6Relay\\logs（仅 GUI 模式生成）"
	}
	line("自动日志: %s", logPath)
	status = append(status, fmt.Sprintf("系统: %s", win.OSVersion()))
	status = append(status, fmt.Sprintf("管理员权限: %v", admin))

	line("")
	line("== 1. 安全软件 ==")
	av := win.SecuritySoftware()
	if len(av) == 0 {
		line("未检测到常见安全软件进程")
		status = append(status, "安全软件: 未检测到")
	} else {
		for _, item := range av {
			line("检测到: %s", item)
		}
		status = append(status, "安全软件: "+strings.Join(av, "、"))
	}

	line("")
	line("== 2. 驱动文件 ==")
	driverDir, err := win.EnsureDriver()
	var files win.DriverStatus
	if err != nil {
		fail("释放驱动文件: %v", err)
		files = win.InspectDriver(win.DriverDir())
		status = append(status, "驱动文件: 释放失败")
	} else {
		files = win.InspectDriver(driverDir)
		line("目录: %s", files.Dir)
		line("WinDivert64.dll: %s", files.DLL.Describe())
		line("WinDivert64.sys: %s", files.SYS.Describe())
		line("许可证: %s", files.License.Describe())
		if files.Complete {
			status = append(status, "驱动文件: 完整且哈希匹配")
		} else {
			ok = false
			status = append(status, "驱动文件: 缺失或哈希不一致")
			line("判断: 驱动文件不完整，通常是杀毒软件隔离/删除了文件。")
			line(avHint(av))
		}
	}

	line("")
	line("== 3. WinDivert 服务 ==")
	svc := win.QueryWinDivertService()
	line("服务: %s", svc.Summary())
	if svc.Exists {
		line("ImagePath: %s", svc.ImagePath)
		line("ImagePath 文件存在: %v", svc.ImageExists)
		if !svc.ImageExists {
			line("判断: 服务残留且指向不存在的文件；管理员执行 sc.exe stop WinDivert 和 sc.exe delete WinDivert 后重试。")
		}
		if svc.External() {
			line("提示: 该服务由其他程序安装（ImagePath 不在系统驱动目录，也不在我们的驱动目录）。多个程序共用 WinDivert 服务时可能出现驱动版本冲突；如反复失败，先退出占用它的程序再重试。")
		}
	}
	status = append(status, "WinDivert 服务: "+svc.Summary())

	line("")
	line("== 4. 驱动加载 ==")
	if driverDir != "" {
		if _, err := win.LoadDriver(driverDir); err != nil {
			fail("LoadLibrary: %v", err)
			status = append(status, "驱动加载: LoadLibrary 失败")
			line(driverErrorHint(errnoOf(err), files, svc, av))
		} else {
			line("LoadLibrary: 成功")
			if err := win.TestDriver(driverDir); err != nil {
				fail("WinDivertOpen: %v", err)
				status = append(status, "驱动加载: WinDivertOpen 失败")
				line(driverErrorHint(errnoOf(err), files, svc, av))
			} else {
				line("WinDivertOpen(\"false\") / Close: 成功")
				status = append(status, "驱动加载: 成功")
			}
		}
	}

	line("")
	line("== 5. Defender 隔离与排除项 ==")
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
					line("隔离记录: %s", item)
				}
			}
		}
		if matches == 0 {
			line("未发现 WinDivert/Civ6Relay 相关隔离记录")
		} else {
			line("共发现 %d 条相关隔离记录", matches)
		}
		excluded := false
		for _, item := range def.Exclusions {
			line("排除项: %s", item)
			lower := strings.ToLower(item)
			if strings.Contains(lower, "civ6relay") || strings.Contains(lower, "windivert") {
				excluded = true
			}
		}
		if !excluded {
			line("驱动目录不在 Defender 排除项中（需要手动添加）")
		} else {
			line("驱动目录已在 Defender 排除项中")
		}
		status = append(status, fmt.Sprintf("Defender: 隔离记录 %d 条, 已排除=%v", matches, excluded))
	}

	line("")
	line("== 6. 网络 ==")
	host, port, err := parseRelayAddress(relayText)
	if err != nil {
		line("中转地址未配置或无效: %v", err)
		status = append(status, "中转地址: 未配置")
	} else {
		line("中转地址: %s", win.FormatRelay(host, port))
		ips, err := net.LookupIP(host)
		var ipv4 net.IP
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				ipv4 = v4
				break
			}
		}
		if err != nil || ipv4 == nil {
			fail("解析中转地址: %v", err)
			status = append(status, "中转解析: 失败")
		} else {
			line("解析结果: %s", ipv4)
			if route, err := win.RouteTo(ipv4); err != nil {
				fail("查询出口: %v", err)
				status = append(status, "本地出口: 失败")
			} else {
				line("本地出口: %s, 网卡索引 %d", route.LocalIP, route.IfIndex)
				status = append(status, "本地出口: "+route.LocalIP.String())
			}
		}
	}
	if subnet, err := win.SelectVirtualSubnet(); err != nil {
		fail("选择虚拟网段: %v", err)
		status = append(status, "虚拟网段: 失败")
	} else {
		line("虚拟网段: %s", subnet)
		status = append(status, "虚拟网段: "+subnet.String())
	}

	line("")
	line("== 7. 防火墙 ==")
	if win.FirewallRuleExists() {
		line("Civ6Relay-Game-Inbound: 已存在")
		status = append(status, "防火墙规则: 已存在")
	} else {
		line("Civ6Relay-Game-Inbound: 未创建（首次连接时会自动创建）")
		status = append(status, "防火墙规则: 未创建")
	}

	line("")
	line("== 8. 结论 ==")
	if ok {
		line("WinDivert 驱动加载正常，可以尝试连接房间。")
	} else {
		line("存在失败项，请按上面的提示处理；如果是杀软拦截，先在隔离区恢复文件再把驱动目录加入信任。")
	}
	line("自动日志: %s", logPath)

	reportPath = filepath.Join(os.TempDir(), "Civ6RelayDiag.txt")
	content := append([]byte{0xEF, 0xBB, 0xBF}, []byte(b.String())...)
	_ = os.WriteFile(reportPath, content, 0o644)
	summary = strings.Join(status, "\n")
	if ok {
		summary += "\n\n诊断通过：WinDivert 驱动可以正常加载。"
	} else {
		summary += "\n\n诊断发现问题，请查看完整报告。"
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
