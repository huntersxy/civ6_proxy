# Civ6Relay

《文明 VI》（*Sid Meier's Civilization VI*）局域网联机的公网 UDP 中继与 P2P 协调工具，使用 Go 实现。

系统由无状态的 Linux 中转服务端与单文件 Windows 客户端组成：客户端通过 WinDivert 在本机旁路捕获并注入游戏 UDP 数据报，实现跨公网的透明局域网联机，无需修改游戏文件或额外配置虚拟网卡驱动。

---

## 架构与协议

### 数据面路径

| 流量类型 | 端口 | 处理方式 |
| --- | --- | --- |
| 房间发现广播 | UDP `62900–62999` | WinDivert 旁路复制并经中转广播，原始广播继续按本地网络发送 |
| 游戏会话单播 | UDP `62056` | 分配独立虚拟子网（`/24`）做透明转发；房主启用 P2P 且打洞成功后直连，其余回退中转 |
| 控制与隧道传输 | UDP `42056`（默认） | 客户端与中转服之间单一 UDP 端口复用控制、数据、路径探测与测速协议 |

### 传输协议（`C6R2` / v2）

- **帧结构**：40 字节帧头 + 负载 + 16 字节 HMAC-SHA256 认证标签；单帧最大游戏负载 1144 字节，超长数据报在两端分片并在 2 秒窗口内乱序重组。
- **安全性与会话**：支持 256 包滑动重放窗口与 NAT 端口漂移自动更新；控制帧采用紧凑 UTF-8 JSON 编码。
- **房间模型**：单房间上限 8 人，10 位房间码标识，状态全内存维护（进程重启即清空）。
- **兼容性**：`C6R2` 不向后兼容旧版 `C6R1` / Python 实现，客户端与服务端需同步部署。

---

## 组件与运行要求

| 产物 | 目标平台 | 依赖与权限 | 说明 |
| --- | --- | --- | --- |
| `Civ6RelayClient.exe` | Windows 10/11 x64 | 管理员权限；内嵌 WinDivert 2.2.2-A | 纯 Go 桌面客户端（基于 `go-gui`，无 Web 运行时） |
| `civ6relay-server` | Linux x64 | 仅 Go 标准库，静态链接 | 公网 UDP 中转服务端 |
| `civ6relay-tunnelcheck` | Linux x64 | 仅 Go 标准库，静态链接 | 协议连通性与合成对局压测 CLI |

构建环境要求 **Go 1.26+**，全目标均在 `CGO_ENABLED=0` 下静态编译。

---

## 中转端部署

前置条件：具备公网 IPv4 地址，且云安全组与主机防火墙均已放行入站 **UDP `42056`**。

### systemd 安装

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin civ6relay || true
sudo install -m 0755 civ6relay-server /usr/local/bin/civ6relay-server
sudo install -m 0644 deploy/civ6relay.service /etc/systemd/system/civ6relay.service
sudo systemctl daemon-reload
sudo systemctl enable --now civ6relay
```

若主机防火墙（如 1Panel）默认丢弃入站 UDP，可附加防火墙 drop-in 单元：

```bash
sudo install -d /etc/systemd/system/civ6relay.service.d
sudo install -m 0644 deploy/civ6relay-firewall.conf /etc/systemd/system/civ6relay.service.d/firewall.conf
sudo systemctl daemon-reload
sudo systemctl restart civ6relay
```

### 服务端参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-listen` | `:42056` | UDP 监听地址（IPv4） |
| `-max-rooms` | `1024` | 最大并发房间数 |
| `-readers` | `min(4, 2×CPU)` | UDP 读取协程数（上限 64） |
| `-member-pps` | `5000` | 单成员每秒 DATA 帧数限额（含 2 秒突发预算） |
| `-member-byte-rate` | `1250000` | 单成员每秒 DATA 线速字节限额（约 10 Mbps） |
| `-speedtest-mbps` | `0` | `C6S1` 带宽压测单向速率上限（`0` 禁用，范围 `1–1000`） |
| `-stats` | `1m` | 周期性统计日志间隔（`0` 禁用） |
| `-quiet` | `false` | 关闭房间生命周期日志 |
| `-version` | `false` | 输出版本号后退出 |

> 广播帧按房间规模放大（8 人房间 1 份广播产生 7 份出站帧），调高限额时需为服务端出口带宽预留余量。`-speedtest-mbps` 启用后与业务共用监听端口，同一时刻仅允许一个压测会话。

### 服务端自检

```bash
# 校验建房、入房、单播/广播、分片重组与 RTT 探测
./civ6relay-tunnelcheck -relay 127.0.0.1:42056

# 运行 30 秒同机双端合成对局测试
./civ6relay-tunnelcheck -relay 127.0.0.1:42056 -playtest
```

---

## 客户端运行与运维

### 核心模块

客户端采用单窗口三视图布局（自动跟随系统浅/深色主题）：

1. **联机大厅**：配置中转地址（自动记录最近 20 个成功建房地址）与昵称，执行建房、10 位房间码入房、房主 P2P 直连开关与防火墙规则同步，实时展示各成员连接路径（`直连` / `打洞中` / `中转`）及真实端到端 RTT。
2. **网络测试**：
   - **联机模拟**：通过 `C6R2` 建立临时房间并双端互通，采集发现应答、双向数据报丢包（2 秒结算窗口）、分片大包送达率及常态/同步 RTT 分布。无需 WinDivert，不干扰当前会话。
   - **带宽压测**：基于 `C6S1` 依次测量空载延迟、10 秒下行与 10 秒上行速率及负载 RTT（需服务端开启 `-speedtest-mbps`）。
3. **诊断与日志**：一键核验 WinDivert 驱动哈希、驱动加载、本地路由出口、虚拟网段冲突、Defender 隔离/排除项及防火墙规则；提供实时运行日志浏览、导出与抓包目录访问。

### 本地文件与路径

| 用途 | 路径 | 说明 |
| --- | --- | --- |
| 配置文件 | `%APPDATA%\Civ6Relay\config.json` | 持久化中转地址、昵称与建房历史；房间码与令牌仅驻留内存 |
| 运行日志 | `%LOCALAPPDATA%\Civ6Relay\logs\client-YYYYMMDD.log` | UTF-8 BOM，含启动环境、驱动检查与错误记录 |
| 房主抓包 | `%LOCALAPPDATA%\Civ6Relay\captures\*.pcap` | 仅房主记录原始 IPv4/UDP 游戏包（不含控制帧），按小时轮转，自动保留 72 小时 |
| 驱动目录 | `%ProgramData%\Civ6Relay\windivert-2.2.2\` | 运行时从 exe 释放 WinDivert DLL/SYS 与许可证 |
| 诊断报告 | `%TEMP%\Civ6RelayDiag.txt` | 界面诊断或 `-diag` 参数生成 |

### 命令行参数

| 参数 | 说明 |
| --- | --- |
| `-diag` | 无窗口执行环境与驱动诊断，写入诊断报告后退出 |
| `-preview` | 跳过管理员提权检查（界面预览与调试） |
| `-no-capture` | 跳过 WinDivert 与防火墙配置，仅测试隧道控制面 |
| `-version` | 显示版本信息后退出 |

### 安全软件与驱动校验

客户端内嵌官方未修改的 **WinDivert 2.2.2-A**（`WinDivert64.sys` 具官方签名，`WinDivert64.dll` 无签名）。部分安全软件（Defender、火绒、360、腾讯管家等）可能隔离驱动导致 `WinDivertOpen` 返回 `WinError 2` / `WinError 5`。可将 `%ProgramData%\Civ6Relay` 与客户端可执行文件加入排除项，并通过诊断页核对 SHA-256：

- `WinDivert64.dll`: `c1e060ee19444a259b2162f8af0f3fe8c4428a1c6f694dce20de194ac8d7d9a2`
- `WinDivert64.sys`: `8da085332782708d8767bcace5327a6ec7283c17cfb85e40b03cd2323a90ddc2`

---

## 构建与发布

### 本地构建

```powershell
.\build.ps1 -Version 2.1.1 -OutputDir dist
```

如需手动分目标构建：

```powershell
go test ./...

$env:GOOS="windows"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"
go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=dev" -o dist/Civ6RelayClient.exe ./cmd/client

$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"
go build -trimpath -ldflags "-s -w -X main.version=dev" -o dist/civ6relay-server ./cmd/server
go build -trimpath -ldflags "-s -w" -o dist/civ6relay-tunnelcheck ./cmd/tunnelcheck
```

若修改了 [cmd/client/app.manifest](cmd/client/app.manifest)，需重新生成嵌入清单资源：

```powershell
go run github.com/akavel/rsrc@latest -manifest cmd/client/app.manifest -o cmd/client/rsrc_windows_amd64.syso -arch amd64
```

### CI 自动发包

推送匹配 `v*` 的 Git Tag 时，[.github/workflows/release.yml](.github/workflows/release.yml) 将自动执行双平台测试，交叉编译 Windows 客户端与 Linux 服务端/自检工具，生成压缩包与 `SHA256SUMS.txt` 并发布 GitHub Release。

```bash
git tag -a v261125 -m "Release v261125"
git push origin v261125
```

---

## 目录结构

```text
cmd/
  client/              Windows 客户端（Go-Gui 界面、会话调度、诊断、配置）
  server/              Linux UDP 中转服务端
  tunnelcheck/         协议与对局模拟自检工具
internal/
  protocol/            C6R2 帧编解码、HMAC-SHA256、重放窗口、分片重组、P2P 控制帧
  relay/               房间状态机、令牌桶限流与转发核心
  tunnel/              客户端 UDP 隧道、心跳、P2P 打洞与路径探测
  playtest/            合成对局流量与时延/丢包统计
  speedtest/           C6S1 UDP 带宽压测服务端与客户端
  pcap/                原始 IPv4 PCAP 记录与过期清理
  packet/              IPv4/UDP 数据报解析与构造
  win/                 WinDivert 加载、流量桥接、路由/防火墙/进程检测
deploy/                systemd 服务单元与防火墙 drop-in 配置
.github/workflows/     CI 测试与 Tag 自动发包工作流
```

---

## 第三方组件与许可

项目使用的第三方组件许可及源码声明见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)：

- **WinDivert 2.2.2-A**：LGPL-3.0-or-later / GPL-2.0-or-later
- **Go-Gui**（`github.com/go-gui-org/go-gui`）：MIT
- **rsrc**（`github.com/akavel/rsrc`，仅构建期清单嵌入）：MIT
