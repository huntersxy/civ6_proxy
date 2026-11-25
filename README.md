# Civ6Relay Go

Civ VI「局域网联机」公网 UDP 中继的 Go 重写版：

- **Windows 客户端单 exe**：原生 Win32 界面（Edit/Button/ListView/Static），**零第三方 GUI 依赖**；官方 WinDivert 2.2.2 x64 驱动内嵌在 exe 内，运行时释放到 `%ProgramData%\Civ6Relay`。
- **Linux 中转服单文件**：只依赖 Go 标准库，静态编译，无数据库、无配置文件。
- **与上游 Python 实现 v1 协议字节兼容**（magic `C6R1`）：Go 客户端可以连 Python 服务器，Python 客户端也可以连 Go 服务器。

## 功能

- 2–8 人房间、10 位房间码、成员列表、在线/离线状态。
- HMAC-SHA256 身份验证、256 包滑动重放窗口、NAT 地址更新。
- 大包分片与 2 秒乱序重组，保持 UDP 语义。
- 转发 `62900–62999` 房间发现广播，同时保留真实局域网广播。
- 透明转发 UDP `62056` 游戏会话，不抢占游戏端口。
- 内置防火墙规则配置、虚拟网段冲突检测、Civ 6 进程检测、RTT/流量统计、日志导出。
- 「诊断」一键检查驱动文件哈希、WinDivert 打开、本地出口、虚拟网段和防火墙规则。

## 构建

要求 Go 1.22+。客户端只支持 Windows 10/11 x64（Civ 6 玩家端本身也是 x64）。

```powershell
.\build.ps1 -Version 1.0.0
```

产物在 `dist/`：

| 文件 | 用途 |
| --- | --- |
| `Civ6RelayClient.exe` | Windows 客户端（单文件，原生界面，需管理员权限） |
| `civ6relay-server` | Linux x64 中转服（静态链接） |
| `civ6relay-tunnelcheck` | Linux 中转服自检工具 |

手动构建：

```powershell
go test ./...
go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/Civ6RelayClient.exe ./cmd/client
$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"
go build -trimpath -ldflags "-s -w" -o dist/civ6relay-server ./cmd/server
go build -trimpath -ldflags "-s -w" -o dist/civ6relay-tunnelcheck ./cmd/tunnelcheck
```

### 客户端资源与 DPI

`cmd/client/app.manifest` 声明 Common Controls v6 和系统 DPI 感知，保证 Win10/11 上是原生现代控件、在高分屏上文字清晰。它通过 `cmd/client/rsrc_windows_amd64.syso` 嵌入 exe（已提交，构建时自动包含）。需要修改清单时重新生成：

```powershell
go run github.com/akavel/rsrc@latest -manifest cmd/client/app.manifest -o cmd/client/rsrc_windows_amd64.syso -arch amd64
```

## 中转服部署

只需要公网 IPv4 和 **UDP 42056**（云安全组 + 主机防火墙都要放行）。房间状态只存在内存里，重启即清空。

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin civ6relay || true
sudo install -m 0755 civ6relay-server /usr/local/bin/civ6relay-server
sudo install -m 0644 deploy/civ6relay.service /etc/systemd/system/civ6relay.service
sudo systemctl daemon-reload
sudo systemctl enable --now civ6relay
journalctl -u civ6relay -f
```

如果主机启用了 1Panel 或其它默认丢弃入站 UDP 的防火墙，再安装一个 drop-in，让服务启动时自动放行 UDP 42056：

```bash
sudo install -d /etc/systemd/system/civ6relay.service.d
sudo install -m 0644 deploy/civ6relay-firewall.conf /etc/systemd/system/civ6relay.service.d/firewall.conf
sudo systemctl daemon-reload
sudo systemctl restart civ6relay
```

也可以直接在 1Panel 的「防火墙」里添加 UDP 42056 规则；两者不冲突。

启动参数：

```text
-listen 0.0.0.0:42056   监听地址（默认 :42056）
-max-rooms 1024         最大房间数
-readers 4              UDP 读取协程数（默认 min(4, 2×CPU)）
-quiet                  关闭房间生命周期日志
-stats 1m               统计日志间隔，0 关闭
```

自检：

```bash
./civ6relay-tunnelcheck -relay 127.0.0.1:42056
# PASS 表示建房、加入、单播、广播全部正常
```

## 客户端使用

1. 把 `Civ6RelayClient.exe` 放到任意纯英文目录（如 `D:\Civ6Relay\`）。
2. 右键 → **以管理员身份运行**（加载 WinDivert 驱动和配置防火墙需要）。
3. 所有人填写同一中转地址（`公网IP:42056`）和各自昵称。
4. 房主点「创建房间」，把 10 位房间码发给朋友；朋友输入房间码点「加入房间」。
5. 确认成员列表全部「在线」后，房主进 Civ 6 → 多人游戏 → 局域网 创建游戏，其他人刷新列表加入。

客户端只把中转地址和昵称保存到 `%APPDATA%\Civ6Relay\config.json`（与 Python 版同一路径/格式）。房间码和令牌不会持久化。关闭窗口会停止抓包、发送离开消息并退出。

### 诊断

- 界面上的「诊断」按钮，或命令行 `Civ6RelayClient.exe -diag`。
- 报告写入 `%TEMP%\Civ6RelayDiag.txt`，包含驱动文件哈希、WinDivert 打开测试、本地出口、虚拟网段、防火墙规则。
- 如果报 `WinError 2` / 找不到文件，通常是杀毒软件隔离了 `WinDivert64.sys`，或系统里残留了指向已删除文件的 WinDivert 服务。报告会明确指出是哪一种。

### 开发者参数

```text
-preview       跳过管理员检查（调试界面用；连接房间仍会失败）
-no-capture    跳过 WinDivert/防火墙，仅测试隧道（调试用）
-version       显示版本
```

## 兼容性说明

协议层与上游 `Dts0/civ6_proxy` v1 完全一致：

- 40 字节帧头 + 负载 + 16 字节 HMAC，`C6R1` magic。
- `CREATE/JOIN/WELCOME/PEERS/PING/PONG/LEAVE/DATA/ERROR/ROOM_CLOSED` 语义一致。
- 控制消息 JSON 使用 ASCII `\uXXXX` 转义，和 Python `ensure_ascii=True` 一致。
- WELCOME 的 `token` 为带 padding 的 URL-safe base64，`room_id` 为 16 位十六进制。
- 房间码字母表、限流参数、离线/过期时间与 Python 版一致。

已通过双向互操作测试：Go 客户端 ↔ Python 服务端、Python 客户端 ↔ Go 服务端，建房/加入/单播/广播全部通过。

## 代码结构

```text
cmd/client            Windows 原生 Win32 客户端
  app.manifest        Common Controls v6 + DPI 感知
  rsrc_windows_amd64.syso  清单资源（rsrc 生成，已提交）
  win32_windows.go   Win32 FFI、控件、字体、剪贴板、保存对话框
  ui_windows.go      主窗口、布局、ListView、定时刷新
  session_windows.go 连接/房间/日志状态机（与 UI 解耦）
  diag_windows.go    诊断报告
cmd/server           中转服
cmd/tunnelcheck      无界面协议自检
internal/protocol    C6R1 帧格式、HMAC、重放窗口、分片重组
internal/relay       房间状态机、限流、清理
internal/tunnel      客户端隧道（UDP、握手、心跳、指标）
internal/win         WinDivert FFI、IPv4/UDP 注入、路由/防火墙/进程
internal/packet      IPv4/UDP 构造与解析（与 Python 输出逐字节一致）
deploy/              systemd 单元与 1Panel 防火墙 drop-in
```

## 故障排查

| 现象 | 处理 |
| --- | --- |
| `WinDivertOpen 失败: [WinError 2]` | 驱动文件被杀软隔离；把 `C:\ProgramData\Civ6Relay` 加入信任，或用「诊断」确认 |
| `WinDivertOpen 失败: [WinError 5]` | 没有管理员权限 |
| 防火墙规则创建失败 | 客户端仍会继续连接，但游戏可能收不到入站包；检查 62056 / 62900–62999 |
| 提示虚拟网段冲突 | 关闭占用 `10.250.250.0/24`、`172.31.250.0/24`、`192.168.250.0/24` 的 VPN |
| 成员在线但看不到房间 | 房主是否创建了「局域网」游戏；客户端日志里发现包计数是否增长 |
| 能看到房间但进不去 | 检查 UDP 62056、Windows 防火墙、版本/DLC/模组一致性 |
| 中转响应超时 | 检查安全组 UDP 42056、服务器进程、运营商 UDP 质量 |

## 第三方组件

- **WinDivert 2.2.2-A**（LGPL-3.0-or-later / GPL-2.0-or-later），官方驱动与 DLL 原样内嵌，运行时代码动态加载；许可证随 exe 释放到驱动目录，详见 `THIRD_PARTY_NOTICES.md`。
- 构建时仅使用 `github.com/akavel/rsrc`（MIT）把清单写入 `.syso`，运行时不依赖任何第三方库。
