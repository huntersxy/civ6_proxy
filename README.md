# Civ6Relay Go

Civ VI「局域网联机」公网 UDP 中继的 Go 重写版：

- **Windows 客户端单 exe**：原生 Win32 界面（Edit/ComboBox/Button/ListView/Static），**零第三方 GUI 依赖**；官方 WinDivert 2.2.2 x64 驱动内嵌在 exe 内，运行时释放到 `%ProgramData%\Civ6Relay`。
- **Linux 中转服单文件**：只依赖 Go 标准库，静态编译，无数据库、无配置文件。
- **Go 原生 v2 协议**（magic `C6R2`）：控制消息使用紧凑 UTF-8 JSON，客户端与中转服可独立编译部署。

## 功能

- 最多 8 人的房间、10 位房间码、成员列表和在线状态。
- HMAC-SHA256 身份验证、256 包滑动重放窗口、NAT 地址更新。
- 大包分片与 2 秒乱序重组，保持 UDP 语义。
- 旁路捕获并中继 `62900–62999` 房间发现广播，原始广播直接走本地网络。
- 透明转发 UDP `62056` 游戏会话，无需占用游戏端口。
- 入房即探测中转 RTT，显示游戏收发计数、桥接丢弃和文明 VI 运行状态。
- 内置防火墙规则配置、虚拟网段冲突检测和按需展开的运行日志。
- 「诊断」一键检查驱动文件哈希、WinDivert 打开、本地出口、虚拟网段和防火墙规则。
- 房主可选开启中转辅助的 P2P UDP 直连；广播及未打洞成功的单播继续走中转。
- 成员列表逐人显示当前连接路径及真实成员间 RTT；直连测 P2P RTT，回退时通过中转探测成员间 RTT。
- 成功创建房间使用过的中转地址会按最近使用顺序保存，支持从地址下拉框快速切换，最多保留 20 个。
- 「网络测试」默认用临时房间模拟联机，测房间发现、游戏消息往返和丢包；带宽压测独立可选。

## 构建

要求 Go 1.22+。客户端只支持 Windows 10/11 x64（Civ 6 玩家端本身也是 x64）。

```powershell
.\build.ps1 -Version 2.1.1 -OutputDir dist\v2.1.1
```

产物默认写入 `dist/`，可用 `-OutputDir` 指定输出目录。

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
-listen 0.0.0.0:42056      监听地址（默认 :42056）
-max-rooms 1024            最大房间数
-readers 4                 UDP 读取协程数（默认 min(4, 2×CPU)）
-member-pps 5000           每成员每秒可接收的 DATA 帧数
-member-byte-rate 1250000  每成员每秒可接收的 DATA 帧字节数（含帧头与认证标签）
-speedtest-mbps 0          测速时每方向的 UDP 发送上限（0 关闭，可设 1–1000 Mbps）
-quiet                     关闭房间生命周期日志
-stats 1m                  输出房间、成员、收发和套接字错误计数，0 关闭
```

两个成员限额独立生效，默认允许约 10 Mbps 的 DATA 帧输入，并支持 2 秒突发预算。调整限额时要给广播预留出口带宽。8 人房间的一帧广播最多产生 7 份出站帧。

如需启用测速，在中转服启动命令末尾添加 `-speedtest-mbps 100`，按服务器带宽调整数值。测速共用监听端口，每台中转服同一时间接受一个测速会话。服务端验证客户端能接收同一端点的回包后才启动下行，控制消息中断约 2.5 秒会结束会话。

自检：

```bash
./civ6relay-tunnelcheck -relay 127.0.0.1:42056
# PASS 表示建房、加入、单播、广播、分片重组和 RTT 探测通过
./civ6relay-tunnelcheck -relay 127.0.0.1:42056 -playtest
# 运行与客户端相同的 30 秒对局模拟，输出发现应答、双向收发丢包及往返延迟
```

转发微基准可以用 `go test ./internal/relay -run '^$' -bench '^BenchmarkForwardData$' -cpu 1,4 -benchmem` 复跑。它测量 512 字节数据帧的签名、验证及中转处理，覆盖单播和 8 人房间广播。该测量在进程内运行。

## 客户端使用

1. 把 `Civ6RelayClient.exe` 放到任意纯英文目录（如 `D:\Civ6Relay\`）。
2. 右键 → **以管理员身份运行**（加载 WinDivert 驱动和配置防火墙需要）。
3. 所有人填写同一中转地址（`公网IP:42056`）和各自昵称。
4. 房主点「创建房间」，用「复制房间码」分享 10 位房间码。朋友输入房间码后点「加入房间」。
5. 确认成员列表全部「在线」后，房主进 Civ 6 → 多人游戏 → 局域网 创建游戏，其他人刷新列表加入。

连接期间可以点「取消连接」。入房后，窗口将房间码设为只读，并显示成员和中转状态。连接失败的原因显示在窗口上方，完整信息可在「查看日志」里查看。

中转地址框带下拉历史。只有成功创建过房间的地址会被记入 `%APPDATA%\Civ6Relay\config.json` 的 `relay_history`，最多 20 个；最近使用的排前面，重复项会去重。加入房间不会把地址加入建房历史。

入房后，房主可选中「启用 P2P 直连」。中转通过回包挑战验证公网 UDP 地址，再用房间令牌派生对端密钥，协调打洞和双向路径探测。单播只对已验证的直连对端择路发送；发现广播始终走中转，未验证或失效的对端自动回退到中转。Windows 防火墙启用时，客户端按当前 UDP 本地端口和已验证对端自动同步入站、出站规则；规则失败可点「配置 P2P 防火墙」重试。关闭系统防火墙仍不能绕过路由器 NAT、CGNAT 或运营商 UDP 策略限制，无法打洞时成员流量会继续经中转。成员列表的「连接 / 延迟」按每个成员显示「直连」「打洞中」或「中转」及对应 RTT；直连时使用对端 P2P 探测，回退时使用经中转往返到该成员的探测，不会用本机到中转服务器的 RTT 代替。

房主创建房间后，客户端会在本机自动记录经游戏转发桥的 IPv4/UDP 包，包括房间发现广播、游戏发送包及成功注入本机的接收包；普通成员不记录。文件为可用 Wireshark 打开的原始 IPv4 PCAP，保存在 `%LOCALAPPDATA%\Civ6Relay\captures`，界面「抓包目录」可直接打开。记录不含中转握手、房间令牌或 P2P 协商控制帧。文件按小时轮转；客户端启动、运行期间每小时及退出时清理超过 72 小时的 `.pcap`，其它文件不会被清理。抓包包含原始游戏负载，注意不要公开分享。

客户端把当前中转地址、昵称和建房地址历史保存到 `%APPDATA%\Civ6Relay\config.json`。房间码和令牌只保存在内存中。关闭窗口会结束会话、写完排队的抓包并释放转发资源。

### 网络测试

在主窗口填好中转地址，点「网络测试」。默认的「联机模拟」通过实际游戏使用的 `C6R2` 中转路径创建临时房间，再以第二个中转客户端加入。测试以临时端口发出 `62900` 房间发现广播并等待单播回包，再通过 `62056` 模拟 30 秒的双向状态更新、操作应答和短时分片同步。测试不占用游戏端口，不使用 WinDivert，也不会加入或退出正在使用的房间。

窗口分方向显示游戏数据报收发和丢包，并分别列出常态与同步期间的消息往返中位数、P95、抖动和应答数。丢包只以等待满 2 秒的已发送数据报为分母；本地发送失败另计。大数据报必须完整重组才记为送达。点「停止」、按 Esc 或关闭测试窗口可结束并保留已采集结果。联机模拟是**同机双端、经远程中转的合成流量**，不等于实机抓包回放，不能验证其他玩家的网络、防火墙、路由或游戏行为。

需要单独测链路容量时，切到「带宽压测」后点「开始压测」。它先测空载延迟，再分别跑 10 秒下行和上行，按收到的反馈逐步加压。压测丢包包含探测带宽时丢失的 UDP 包，不能拿来估算联机丢包。带宽压测需要中转启用 `C6S1`；联机模拟不需要。

### 诊断

- 界面上的「诊断」按钮，或命令行 `Civ6RelayClient.exe -diag`。
- 报告写入 `%TEMP%\Civ6RelayDiag.txt`，包含驱动文件哈希、WinDivert 打开测试、本地出口、虚拟网段、防火墙规则。
- 如果报 `WinError 2` / 找不到文件，通常是杀毒软件隔离了 `WinDivert64.sys`，或系统里残留了指向已删除文件的 WinDivert 服务。报告会明确指出是哪一种。

### 开发者参数

```text
-preview       跳过管理员检查（调试界面）
-no-capture    跳过 WinDivert/防火墙，仅测试隧道（调试用）
-version       显示版本
```

### 杀软误报与日志（火绒 / 360 / 腾讯管家 / Defender）

客户端使用官方 WinDivert 2.2.2-A：`WinDivert64.sys` 有有效签名，`WinDivert64.dll` 没有签名。火绒、360、腾讯管家、Defender 常把 WinDivert 误报为 HackTool，典型现象是驱动文件在释放后被隔离，客户端报 `WinError 2` 或 `WinError 5`。

- **自动日志**：`%LOCALAPPDATA%\Civ6Relay\logs\client-YYYYMMDD.log`（UTF-8 BOM）。启动环境、管理员状态、检测到的安全软件、驱动文件哈希、WinDivert 服务 ImagePath 和连接错误会写入日志。
- **诊断报告**：界面「诊断」或 `Civ6RelayClient.exe -diag` 生成 `%TEMP%\Civ6RelayDiag.txt`，包含驱动哈希、服务状态/ImagePath、AV 检测、Defender 隔离记录与排除项、网络和防火墙检查。
- **火绒**：防护中心 → 隔离区恢复 `WinDivert64.dll` / `WinDivert64.sys`；安全设置 → 信任区添加 `C:\ProgramData\Civ6Relay` 和 `Civ6RelayClient.exe`；重启客户端。
- **360 / 腾讯管家 / Defender**：在各自隔离区恢复文件，把驱动目录加入信任/排除项后重启客户端。
- 官方哈希（与 reqrypt.org 2.2.2-A 一致）：
  - `WinDivert64.dll` `c1e060ee19444a259b2162f8af0f3fe8c4428a1c6f694dce20de194ac8d7d9a2`
  - `WinDivert64.sys` `8da085332782708d8767bcace5327a6ec7283c17cfb85e40b03cd2323a90ddc2`
- 如果系统里已有其它程序安装的 WinDivert 服务（`ImagePath` 指向别的软件目录），可能与当前 DLL 版本冲突；诊断报告和错误弹窗会明确标出。
- 反馈问题时请附上自动日志和诊断报告。日志可能包含中转地址、昵称、房间码，公开前请自行检查。

## 协议说明

当前协议为 v2（`C6R2` magic、版本字段 `2`），不兼容旧版 Python 客户端和 `C6R1` 中转服。个人部署时，客户端与中转服必须一起更新到本版本；旧帧会被直接拒绝，不做协议降级。

- 40 字节帧头 + 负载 + 16 字节 HMAC。
- 控制消息为紧凑 UTF-8 JSON，避免将中文扩张成 ASCII `\uXXXX` 序列。
- `CREATE/JOIN/WELCOME/PEERS/PING/PONG/PATH_PING/PATH_PONG/LEAVE/DATA/ERROR/ROOM_CLOSED` 与 P2P 控制帧统一使用 v2 帧格式。
- WELCOME 的 `token` 为带 padding 的 URL-safe base64，`room_id` 为 16 位十六进制。
- 单帧最多承载 1144 字节游戏数据。更大的 UDP 数据报在两端分片与重组，中转服按帧转发。
- 成员名单须能装进一个控制帧；昵称过长导致超限时服务端会提示缩短昵称。

## 代码结构

```text
cmd/client            Windows 原生 Win32 客户端
  app.manifest        Common Controls v6 + DPI 感知
  rsrc_windows_amd64.syso  清单资源（rsrc 生成，已提交）
  win32_windows.go   Win32 FFI、控件、字体、剪贴板、保存对话框
  ui_windows.go      主窗口、表单操作、会话状态刷新
  ui_layout_windows.go 控件布局、缩放、成员列表和连接延迟
  speed_windows.go   网络测试窗口与带宽压测视图
  play_windows.go    联机模拟视图
  session_windows.go 连接/房间/日志/抓包生命周期（与 UI 解耦）
  diag_windows.go    诊断报告
cmd/server           中转服
cmd/tunnelcheck      无界面协议自检
internal/protocol    C6R2 帧格式、HMAC、重放窗口、分片重组、P2P 控制帧
internal/relay       房间状态机、限流、清理
internal/speedtest   UDP 测速、速率控制、收包统计
internal/playtest    合成对局流量和送达、往返统计
internal/pcap        原始 IPv4 PCAP 写入与 72 小时保留清理
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
