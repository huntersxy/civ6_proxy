//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"civ6relay/internal/win"
)

// collectDiagnostics gathers driver, network and firewall facts, writes a
// report to %TEMP%\Civ6RelayDiag.txt, and returns a short summary.
func collectDiagnostics(relayText string) (summary, report, reportPath string, ok bool) {
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
	admin := win.IsAdmin()
	line("管理员权限: %v", admin)
	status = append(status, fmt.Sprintf("管理员权限: %v", admin))

	line("")
	line("== 1. 驱动文件 ==")
	driverDir, err := win.EnsureDriver()
	if err != nil {
		fail("释放驱动文件: %v", err)
		status = append(status, "驱动文件: 失败")
	} else {
		line("目录: %s", driverDir)
		expectedDLL, expectedSYS := win.DriverHashes()
		filesOK := true
		for _, item := range []struct {
			name     string
			expected [32]byte
		}{
			{"WinDivert64.dll", expectedDLL},
			{"WinDivert64.sys", expectedSYS},
		} {
			path := filepath.Join(driverDir, item.name)
			data, err := os.ReadFile(path)
			if err != nil {
				filesOK = false
				line("%s: 缺失 (%v)", item.name, err)
				continue
			}
			actual := sha256.Sum256(data)
			match := actual == item.expected
			if !match {
				filesOK = false
			}
			line("%s: %d 字节, sha256=%s, 匹配=%v", item.name, len(data), hex.EncodeToString(actual[:8]), match)
		}
		if filesOK {
			status = append(status, "驱动文件: 完整")
		} else {
			ok = false
			status = append(status, "驱动文件: 异常")
		}
	}

	line("")
	line("== 2. 驱动加载 ==")
	if driverDir != "" {
		if _, err := win.LoadDriver(driverDir); err != nil {
			fail("LoadLibrary: %v", err)
			status = append(status, "驱动加载: 失败")
		} else {
			line("LoadLibrary: 成功")
			if err := win.TestDriver(driverDir); err != nil {
				fail("WinDivertOpen: %v", err)
				status = append(status, "驱动加载: WinDivertOpen 失败")
				line("提示: WinError 2 / 找不到文件通常表示 WinDivert64.sys 被杀毒软件隔离，或系统里残留了指向已删除文件的 WinDivert 服务。")
			} else {
				line("WinDivertOpen(\"false\") / Close: 成功")
				status = append(status, "驱动加载: 成功")
			}
		}
	}

	line("")
	line("== 3. 网络 ==")
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
	line("== 4. 防火墙 ==")
	if win.FirewallRuleExists() {
		line("Civ6Relay-Game-Inbound: 已存在")
		status = append(status, "防火墙规则: 已存在")
	} else {
		line("Civ6Relay-Game-Inbound: 未创建（首次连接时会自动创建）")
		status = append(status, "防火墙规则: 未创建")
	}

	line("")
	line("== 5. 结论 ==")
	if ok {
		line("WinDivert 驱动加载正常，可以尝试连接房间。")
	} else {
		line("存在失败项，请根据上面的错误处理；如为杀毒软件拦截，请把驱动目录加入信任后重试。")
	}

	reportPath = filepath.Join(os.TempDir(), "Civ6RelayDiag.txt")
	_ = os.WriteFile(reportPath, []byte(b.String()), 0o644)
	summary = strings.Join(status, "\n")
	if ok {
		summary += "\n\n诊断通过：WinDivert 驱动可以正常加载。"
	} else {
		summary += "\n\n诊断发现问题，请查看完整报告。"
	}
	return summary, b.String(), reportPath, ok
}

func runDiagStandalone() {
	cfg := loadConfig()
	summary, _, path, ok := collectDiagnostics(win.FormatRelay(cfg.RelayHost, cfg.RelayPort))
	icon := uint32(mbIconInformation)
	if !ok {
		icon = mbIconError
	}
	messageBox("Civ6Relay 诊断", summary+"\n\n完整报告已保存:\n"+path, icon)
}
