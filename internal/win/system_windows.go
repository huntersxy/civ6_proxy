//go:build windows

package win

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
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
func SelectVirtualSubnet() (*net.IPNet, error) {
	used := localIPv4Subnets()
	for _, candidate := range virtualSubnetCandidates {
		_, network, err := net.ParseCIDR(candidate)
		if err != nil {
			continue
		}
		conflict := false
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
func EnsureFirewallRule(subnet *net.IPNet) error {
	const name = "Civ6Relay-Game-Inbound"
	_, _ = runHiddenOutput("netsh", "advfirewall", "firewall", "delete", "rule", "name="+name)
	output, err := runHiddenOutput("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+name,
		"dir=in",
		"action=allow",
		"protocol=UDP",
		"localport=62056,62900-62999",
		"remoteip="+subnet.String(),
		"profile=any",
		"enable=yes",
	)
	if err != nil {
		msg := strings.TrimSpace(output)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("配置 Windows 防火墙失败: %s", msg)
	}
	return nil
}

// Process is one running Civ 6 process.
type Process struct {
	PID  uint32
	Name string
}

// FindCiv6Processes lists CivilizationVI.exe processes using Toolhelp32.
func FindCiv6Processes() []Process {
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
		name := syscall.UTF16ToString(entry.ExeFile[:])
		lower := strings.ToLower(name)
		if lower == "civilizationvi.exe" || lower == "civilizationvi_dx12.exe" {
			procs = append(procs, Process{PID: entry.ProcessID, Name: name})
		}
		entry.Size = uint32(unsafe.Sizeof(entry))
		r, _, _ = process32Next.Call(snap, uintptr(unsafe.Pointer(&entry)))
	}
	return procs
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
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
