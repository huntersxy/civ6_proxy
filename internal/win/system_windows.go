//go:build windows

package win

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// IsAdmin reports whether the current process is elevated.
func IsAdmin() bool {
	shell32 := syscall.NewLazyDLL("shell32.dll")
	proc := shell32.NewProc("IsUserAnAdmin")
	r, _, _ := proc.Call()
	return r != 0
}

// RelaunchAsAdmin starts the current executable elevated and returns nil when
// the user accepted the UAC prompt.
func RelaunchAsAdmin(args string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, err := syscall.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	var params *uint16
	if strings.TrimSpace(args) != "" {
		params, err = syscall.UTF16PtrFromString(args)
		if err != nil {
			return err
		}
	}
	cwd, _ := syscall.UTF16PtrFromString("")
	shell32 := syscall.NewLazyDLL("shell32.dll")
	proc := shell32.NewProc("ShellExecuteW")
	r, _, _ := proc.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		uintptr(unsafe.Pointer(cwd)),
		1, // SW_SHOWNORMAL
	)
	if r <= 32 {
		return fmt.Errorf("以管理员身份启动失败 (ShellExecute=%d)", r)
	}
	return nil
}

// RouteInfo describes the local path to a remote address.
type RouteInfo struct {
	LocalIP net.IP
	IfIndex uint32
}

type sockaddrIn struct {
	Family uint16
	Port   uint16
	Addr   [4]byte
	Zero   [8]byte
}

// RouteTo resolves the local IPv4 address and interface index used to reach
// remote, matching GetBestInterfaceEx semantics used by the Python client.
func RouteTo(remote net.IP) (RouteInfo, error) {
	dst := remote.To4()
	if dst == nil {
		return RouteInfo{}, fmt.Errorf("需要 IPv4 地址")
	}
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IP(dst), Port: 9})
	if err != nil {
		return RouteInfo{}, fmt.Errorf("探测本地出口失败: %w", err)
	}
	local, _ := conn.LocalAddr().(*net.UDPAddr)
	_ = conn.Close()
	if local == nil || local.IP == nil {
		return RouteInfo{}, fmt.Errorf("无法确定本地出口地址")
	}
	info := RouteInfo{LocalIP: local.IP.To4()}
	iphlpapi := syscall.NewLazyDLL("iphlpapi.dll")
	proc := iphlpapi.NewProc("GetBestInterfaceEx")
	var sa sockaddrIn
	sa.Family = syscall.AF_INET
	copy(sa.Addr[:], dst)
	var index uint32
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&index)))
	if r == 0 {
		info.IfIndex = index
	}
	if info.IfIndex == 0 {
		info.IfIndex = interfaceIndexForIP(info.LocalIP)
	}
	if info.IfIndex == 0 {
		return RouteInfo{}, fmt.Errorf("无法确定出口网卡索引")
	}
	return info, nil
}

// SelectLocalRoute resolves the local IPv4 injection route for remote,
// preferring a physical/non-TUN LAN interface when the default route points to
// a proxy TUN fake-IP range (198.18.0.0/15).
func SelectLocalRoute(remote net.IP, excluded ...*net.IPNet) (RouteInfo, bool, error) {
	route, err := RouteTo(remote)
	if err != nil {
		return RouteInfo{}, false, err
	}
	if !isTunnelFakeIP(route.LocalIP) {
		return route, false, nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return route, false, nil
	}
	if fallback, ok := pickNonTunnelRoute(ifaces, excluded); ok {
		return fallback, true, nil
	}
	return route, false, nil
}

func isTunnelFakeIP(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 198 && (v4[1] == 18 || v4[1] == 19)
}

func pickNonTunnelRoute(ifaces []net.Interface, excluded []*net.IPNet) (RouteInfo, bool) {
	bestPriority := 0
	var best RouteInfo
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if isLikelyTunnelInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			v4 := ipnet.IP.To4()
			if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() || isTunnelFakeIP(v4) {
				continue
			}
			skip := false
			for _, ex := range excluded {
				if ex != nil && ex.Contains(v4) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			priority := localIPPriority(v4)
			if priority > bestPriority {
				bestPriority = priority
				best = RouteInfo{LocalIP: v4, IfIndex: uint32(iface.Index)}
			}
		}
	}
	return best, bestPriority > 0
}

func isLikelyTunnelInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, token := range []string{"mihomo", "clash", "wintun", "sing-box", "tun", "tap", "wireguard", "tailscale", "radmin", "zerotier"} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

func localIPPriority(ip net.IP) int {
	v4 := ip.To4()
	if v4 == nil {
		return 0
	}
	switch {
	case v4[0] == 192 && v4[1] == 168:
		return 4
	case v4[0] == 10:
		return 3
	case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31:
		return 2
	case v4.IsGlobalUnicast():
		return 1
	default:
		return 0
	}
}

func interfaceIndexForIP(ip net.IP) uint32 {
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
				return uint32(iface.Index)
			}
		}
	}
	return 0
}

var virtualSubnetCandidates = []string{
	"10.250.250.0/24",
	"172.31.250.0/24",
	"192.168.250.0/24",
}

// SelectVirtualSubnet picks a /24 that does not overlap any local subnet or
// IPv4 route.
func SelectVirtualSubnet(reserved ...net.IP) (*net.IPNet, error) {
	used := localIPv4Subnets()
	for _, candidate := range virtualSubnetCandidates {
		_, network, err := net.ParseCIDR(candidate)
		if err != nil {
			continue
		}
		conflict := false
		for _, address := range reserved {
			if network.Contains(address) {
				conflict = true
				break
			}
		}
		for _, existing := range used {
			ones, _ := existing.Mask.Size()
			if ones == 0 {
				continue
			}
			if networksOverlap(network, existing) {
				conflict = true
				break
			}
		}
		if !conflict {
			return network, nil
		}
	}
	return nil, fmt.Errorf("所有保留虚拟网段都与本机路由冲突，请先关闭相关 VPN")
}

func networksOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func localIPv4Subnets() []*net.IPNet {
	var out []*net.IPNet
	seen := make(map[string]bool)
	add := func(n *net.IPNet) {
		if n == nil || n.IP.To4() == nil {
			return
		}
		key := n.String()
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, n)
	}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				add(&net.IPNet{IP: ipnet.IP.To4(), Mask: ipnet.Mask})
			}
		}
	}
	// route.exe is present on every supported Windows version and its table
	// is not localized for destination/mask columns.
	if output, err := runHiddenOutput("route", "print", "-4"); err == nil {
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			ip := net.ParseIP(fields[0]).To4()
			maskIP := net.ParseIP(fields[1]).To4()
			if ip == nil || maskIP == nil {
				continue
			}
			mask := net.IPMask(maskIP)
			if ones, bits := mask.Size(); ones == 0 || bits != 32 {
				continue
			}
			add(&net.IPNet{IP: ip.Mask(mask), Mask: mask})
		}
	}
	return out
}

// EnsureFirewallRule creates or refreshes the inbound UDP rule for the
// selected virtual subnet.
func EnsureFirewallRule(ctx context.Context, subnet *net.IPNet) error {
	const name = "Civ6Relay-Game-Inbound"
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	config := []string{
		"action=allow",
		"protocol=UDP",
		"localport=62056,62900-62999",
		"remoteip=" + subnet.String(),
		"profile=any",
		"enable=yes",
	}
	update := append([]string{"advfirewall", "firewall", "set", "rule", "name=" + name, "dir=in", "new"}, config...)
	if _, err := runHiddenOutputContext(ctx, "netsh", update...); err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("配置 Windows 防火墙失败: %w", ctx.Err())
	}
	create := append([]string{"advfirewall", "firewall", "add", "rule", "name=" + name, "dir=in"}, config...)
	output, err := runHiddenOutputContext(ctx, "netsh", create...)
	if err != nil {
		msg := strings.TrimSpace(output)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("配置 Windows 防火墙失败: %s", msg)
	}
	return nil
}

func EnsureP2PFirewallRule(ctx context.Context, localPort uint16, peers []netip.Addr) error {
	const (
		legacyName   = "Civ6Relay-P2P-Peers"
		inboundName  = "Civ6Relay-P2P-Inbound"
		outboundName = "Civ6Relay-P2P-Outbound"
	)
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if localPort == 0 || len(peers) == 0 {
		for _, name := range []string{legacyName, inboundName, outboundName} {
			if err := deleteFirewallRule(ctx, name); err != nil {
				return fmt.Errorf("移除 P2P 防火墙规则失败: %w", err)
			}
		}
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法确定客户端程序路径: %w", err)
	}
	seen := make(map[string]struct{}, len(peers))
	addresses := make([]string, 0, len(peers))
	for _, peer := range peers {
		ip := peer.Unmap()
		if !ip.Is4() || !ip.IsGlobalUnicast() {
			continue
		}
		address := ip.String()
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return EnsureP2PFirewallRule(ctx, 0, nil)
	}
	slices.Sort(addresses)
	baseConfig := []string{
		"action=allow",
		`program="` + executable + `"`,
		"protocol=UDP",
		"localport=" + strconv.Itoa(int(localPort)),
		"remoteip=" + strings.Join(addresses, ","),
		"profile=any",
		"enable=yes",
	}
	for _, rule := range []struct {
		name      string
		direction string
	}{{inboundName, "in"}, {outboundName, "out"}} {
		update := append([]string{"advfirewall", "firewall", "set", "rule", "name=" + rule.name,
			"dir=" + rule.direction, "new"}, baseConfig...)
		if _, err := runHiddenOutputContext(ctx, "netsh", update...); err == nil {
			continue
		}
		if ctx.Err() != nil {
			return fmt.Errorf("配置 P2P 防火墙规则失败: %w", ctx.Err())
		}
		create := append([]string{"advfirewall", "firewall", "add", "rule", "name=" + rule.name,
			"dir=" + rule.direction}, baseConfig...)
		output, err := runHiddenOutputContext(ctx, "netsh", create...)
		if err != nil {
			msg := strings.TrimSpace(output)
			if msg == "" {
				msg = err.Error()
			}
			return fmt.Errorf("配置 P2P %s 防火墙规则失败: %s", map[string]string{"in": "入站", "out": "出站"}[rule.direction], msg)
		}
	}
	return deleteFirewallRule(ctx, legacyName)
}

func deleteFirewallRule(ctx context.Context, name string) error {
	output, showErr := runHiddenOutputContext(ctx, "netsh", "advfirewall", "firewall", "show", "rule", "name="+name)
	if showErr != nil {
		message := strings.ToLower(strings.TrimSpace(output))
		if strings.Contains(message, "no rules match") || strings.Contains(message, "no rule match") ||
			(strings.Contains(message, "匹配") && (strings.Contains(message, "没有") || strings.Contains(message, "找不到") || strings.Contains(message, "未找到"))) {
			return nil
		}
		if output == "" {
			return fmt.Errorf("检查 %s: %w", name, showErr)
		}
		return fmt.Errorf("检查 %s: %s", name, strings.TrimSpace(output))
	}
	if !strings.Contains(strings.ToLower(output), strings.ToLower(name)) {
		return nil
	}
	output, err := runHiddenOutputContext(ctx, "netsh", "advfirewall", "firewall", "delete", "rule", "name="+name)
	if err != nil {
		message := strings.TrimSpace(output)
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("删除 %s: %s", name, message)
	}
	return nil
}

// Process is one running Civ 6 process.
type Process struct {
	PID  uint32
	Name string
}

// listProcesses returns all running process names using Toolhelp32.
func listProcesses() []Process {
	const (
		th32csSnapProcess = 0x00000002
		invalidHandle     = ^uintptr(0)
	)
	type processEntry32 struct {
		Size            uint32
		CntUsage        uint32
		ProcessID       uint32
		DefaultHeapID   uintptr
		ModuleID        uint32
		CntThreads      uint32
		ParentProcessID uint32
		PriClassBase    int32
		Flags           uint32
		ExeFile         [260]uint16
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createSnapshot := kernel32.NewProc("CreateToolhelp32Snapshot")
	process32First := kernel32.NewProc("Process32FirstW")
	process32Next := kernel32.NewProc("Process32NextW")
	closeHandle := kernel32.NewProc("CloseHandle")

	snap, _, _ := createSnapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == invalidHandle {
		return nil
	}
	defer closeHandle.Call(snap)
	var entry processEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	var procs []Process
	r, _, _ := process32First.Call(snap, uintptr(unsafe.Pointer(&entry)))
	for r != 0 {
		procs = append(procs, Process{PID: entry.ProcessID, Name: syscall.UTF16ToString(entry.ExeFile[:])})
		entry.Size = uint32(unsafe.Sizeof(entry))
		r, _, _ = process32Next.Call(snap, uintptr(unsafe.Pointer(&entry)))
	}
	return procs
}

// FindCiv6Processes lists CivilizationVI.exe processes.
func FindCiv6Processes() []Process {
	var out []Process
	for _, p := range listProcesses() {
		lower := strings.ToLower(p.Name)
		if lower == "civilizationvi.exe" || lower == "civilizationvi_dx12.exe" {
			out = append(out, p)
		}
	}
	return out
}

// FirewallRuleExists reports whether the Civ6Relay inbound rule is present.
func FirewallRuleExists() bool {
	_, err := runHiddenOutput("netsh", "advfirewall", "firewall", "show", "rule", "name=Civ6Relay-Game-Inbound")
	return err == nil
}

// VirtualMemberIP returns the synthetic member address inside subnet.
func VirtualMemberIP(subnet *net.IPNet, memberID uint8) net.IP {
	base := subnet.IP.To4()
	if base == nil {
		return nil
	}
	return net.IPv4(base[0], base[1], base[2], memberID)
}

// FormatRelay formats host:port for logs and UI.
func FormatRelay(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func runHiddenOutput(name string, args ...string) (string, error) {
	return runHiddenOutputTimeout(5*time.Second, name, args...)
}

func runHiddenOutputContext(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// SecuritySoftware detects common Chinese security products and Defender.
// The returned strings include the matched process name for logs.
func SecuritySoftware() []string {
	detected := make(map[string]string)
	for _, p := range listProcesses() {
		lower := strings.ToLower(p.Name)
		switch {
		case strings.HasPrefix(lower, "hips"):
			detected["火绒"] = p.Name
		case strings.HasPrefix(lower, "360") || lower == "zhudongfangyu.exe":
			detected["360 安全卫士"] = p.Name
		case strings.HasPrefix(lower, "qqpc") || strings.HasPrefix(lower, "qqpcmgr"):
			detected["腾讯电脑管家"] = p.Name
		case lower == "msmpeng.exe":
			detected["Microsoft Defender"] = p.Name
		case strings.HasPrefix(lower, "kxe"):
			detected["金山毒霸"] = p.Name
		}
	}
	dirs := []struct {
		name string
		path []string
	}{
		{"火绒", []string{`C:\Program Files\Huorong\Sysdiag`, `C:\Program Files (x86)\Huorong\Sysdiag`}},
		{"360 安全卫士", []string{`C:\Program Files\360\360Safe`, `C:\Program Files (x86)\360\360Safe`}},
		{"腾讯电脑管家", []string{`C:\Program Files\Tencent\QQPCMgr`, `C:\Program Files (x86)\Tencent\QQPCMgr`}},
	}
	for _, item := range dirs {
		if _, exists := detected[item.name]; exists {
			continue
		}
		for _, path := range item.path {
			if _, err := os.Stat(path); err == nil {
				detected[item.name] = ""
				break
			}
		}
	}
	order := []string{"火绒", "360 安全卫士", "腾讯电脑管家", "Microsoft Defender", "金山毒霸"}
	var out []string
	for _, name := range order {
		if proc, ok := detected[name]; ok {
			if proc != "" {
				out = append(out, name+" ("+proc+")")
			} else {
				out = append(out, name)
			}
		}
	}
	return out
}

type osVersionInfoW struct {
	Size     uint32
	Major    uint32
	Minor    uint32
	Build    uint32
	Platform uint32
	CSD      [128]uint16
}

// OSVersion returns the Windows version using RtlGetVersion.
func OSVersion() string {
	ntdll := syscall.NewLazyDLL("ntdll.dll")
	proc := ntdll.NewProc("RtlGetVersion")
	var info osVersionInfoW
	info.Size = uint32(unsafe.Sizeof(info))
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(&info)))
	if uint32(r) == 0 {
		return fmt.Sprintf("Windows %d.%d.%d", info.Major, info.Minor, info.Build)
	}
	return "Windows (unknown)"
}

// WinDivertService describes the installed WinDivert kernel service.
type WinDivertService struct {
	Exists      bool
	State       string
	ImagePath   string
	ImageExists bool
	Output      string
}

// QueryWinDivertService inspects the WinDivert service and its driver path.
func QueryWinDivertService() WinDivertService {
	var svc WinDivertService
	out, err := runHiddenOutput("sc.exe", "qc", "WinDivert")
	svc.Output = strings.TrimSpace(out)
	if err != nil {
		return svc
	}
	svc.Exists = true
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "BINARY_PATH_NAME") {
			if idx := strings.Index(line, ":"); idx >= 0 {
				path := strings.TrimSpace(line[idx+1:])
				path = strings.TrimPrefix(path, `\??\`)
				svc.ImagePath = path
			}
		}
	}
	if svc.ImagePath != "" {
		if _, err := os.Stat(svc.ImagePath); err == nil {
			svc.ImageExists = true
		}
	}
	if qout, err := runHiddenOutput("sc.exe", "query", "WinDivert"); err == nil {
		for _, line := range strings.Split(qout, "\n") {
			if strings.Contains(line, "STATE") {
				svc.State = strings.TrimSpace(line)
				break
			}
		}
	}
	return svc
}

// External reports whether the service's driver lives outside the standard
// Windows driver directory and our extracted driver directory. That usually
// means another application installed its own WinDivert build.
func (s WinDivertService) External() bool {
	if !s.Exists || s.ImagePath == "" {
		return false
	}
	lower := strings.ToLower(filepath.Clean(s.ImagePath))
	system := strings.ToLower(filepath.Clean(os.Getenv("SystemRoot") + `\System32\drivers`))
	ours := strings.ToLower(filepath.Clean(DriverDir()))
	return !strings.HasPrefix(lower, system+`\`) && !strings.HasPrefix(lower, ours+`\`)
}

// Summary formats the service state for logs and diagnostics.
func (s WinDivertService) Summary() string {
	if !s.Exists {
		return "未安装"
	}
	path := s.ImagePath
	if path == "" {
		path = "未知"
	}
	file := "文件存在"
	if !s.ImageExists {
		file = "文件缺失"
	}
	state := s.State
	if state == "" {
		state = "状态未知"
	}
	return fmt.Sprintf("%s；ImagePath=%s（%s）", state, path, file)
}

// DefenderInfo is the result of querying Defender quarantine and exclusions.
type DefenderInfo struct {
	Queried    bool
	Quarantine []string
	Exclusions []string
	Error      string
}

// QueryDefender reads Defender detection history and configured exclusions.
func QueryDefender() DefenderInfo {
	script := `$ErrorActionPreference='SilentlyContinue'; ` +
		`Get-MpThreatDetection | Sort-Object InitialDetectionTime -Descending | Select-Object -First 20 | ForEach-Object { 'DET|' + (($_.Resources) -join ';') }; ` +
		`(Get-MpPreference).ExclusionPath | ForEach-Object { 'EXC|' + $_ }`
	out, err := runHiddenOutputTimeout(10*time.Second, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	info := DefenderInfo{Queried: true}
	if err != nil {
		info.Error = err.Error()
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DET|") {
			if value := strings.TrimSpace(strings.TrimPrefix(line, "DET|")); value != "" {
				info.Quarantine = append(info.Quarantine, value)
			}
		} else if strings.HasPrefix(line, "EXC|") {
			if value := strings.TrimSpace(strings.TrimPrefix(line, "EXC|")); value != "" {
				info.Exclusions = append(info.Exclusions, value)
			}
		}
	}
	return info
}

func runHiddenOutputTimeout(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := runHiddenOutputContext(ctx, name, args...)
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("命令超时 (%s)", timeout)
	}
	return out, err
}
