//go:build windows

package win

import (
	"crypto/sha256"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"civ6relay/internal/packet"
)

func TestRouteAndSubnet(t *testing.T) {
	route, err := RouteTo(net.IPv4(223, 5, 5, 5))
	if err != nil {
		t.Fatalf("RouteTo: %v", err)
	}
	if route.LocalIP == nil || route.IfIndex == 0 {
		t.Fatalf("incomplete route: %+v", route)
	}
	subnet, err := SelectVirtualSubnet()
	if err != nil {
		t.Fatalf("SelectVirtualSubnet: %v", err)
	}
	if ones, bits := subnet.Mask.Size(); ones != 24 || bits != 32 {
		t.Fatalf("unexpected subnet %v", subnet)
	}
	ip := VirtualMemberIP(subnet, 3)
	if ip == nil || !subnet.Contains(ip) {
		t.Fatalf("bad virtual member ip %v", ip)
	}
}

func TestDriverExtractAndLoad(t *testing.T) {
	dir, err := EnsureDriver()
	if err != nil {
		t.Fatalf("EnsureDriver: %v", err)
	}
	for _, name := range []string{windivertDLLName, windivertSYSName, windivertLicenseName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	dllBytes, err := os.ReadFile(filepath.Join(dir, windivertDLLName))
	if err != nil {
		t.Fatal(err)
	}
	sysBytes, err := os.ReadFile(filepath.Join(dir, windivertSYSName))
	if err != nil {
		t.Fatal(err)
	}
	expectedDLL, expectedSYS := DriverHashes()
	if sha256.Sum256(dllBytes) != expectedDLL {
		t.Fatal("extracted DLL hash mismatch")
	}
	if sha256.Sum256(sysBytes) != expectedSYS {
		t.Fatal("extracted SYS hash mismatch")
	}
	dll, err := LoadDriver(dir)
	if err != nil {
		t.Fatalf("LoadDriver: %v", err)
	}
	if _, err := loadDivertAPI(dll); err != nil {
		t.Fatalf("missing exports: %v", err)
	}
	t.Run("bridge filters", func(t *testing.T) {
		eval, err := dll.FindProc("WinDivertHelperEvalFilter")
		if err != nil {
			t.Fatal(err)
		}
		_, subnet, err := net.ParseCIDR("10.250.250.0/24")
		if err != nil {
			t.Fatal(err)
		}
		game, discovery := bridgeFilters(subnet)
		for _, test := range []struct {
			target    net.IP
			port      uint16
			game      bool
			discovery bool
		}{
			{VirtualMemberIP(subnet, 2), 62056, true, false},
			{VirtualMemberIP(subnet, 9), 62056, true, false},
			{VirtualMemberIP(subnet, 10), 62056, false, false},
			{net.IPv4bcast, 62900, false, true},
			{net.IPv4bcast, 62999, false, true},
			{net.IPv4bcast, 62056, false, false},
		} {
			raw, err := packet.BuildIPv4UDP(net.IPv4(10, 0, 0, 1), test.target, 62056, test.port, nil, 128, 1)
			if err != nil {
				t.Fatal(err)
			}
			address := wdAddress{Bits: 1 << 17}
			matches := func(filter string) bool {
				expr, err := syscall.BytePtrFromString(filter)
				if err != nil {
					t.Fatal(err)
				}
				result, _, _ := eval.Call(uintptr(unsafe.Pointer(expr)), uintptr(unsafe.Pointer(&raw[0])),
					uintptr(len(raw)), uintptr(unsafe.Pointer(&address)))
				return result != 0
			}
			if matches(game) != test.game || matches(discovery) != test.discovery {
				t.Errorf("unexpected capture rule for %s:%d", test.target, test.port)
			}
		}
	})
}

func TestListProcesses(t *testing.T) {
	procs := listProcesses()
	if len(procs) < 10 {
		t.Fatalf("expected many processes, got %d", len(procs))
	}
	_ = SecuritySoftware()
}

func TestTunnelFakeIPAndLocalPriority(t *testing.T) {
	if !isTunnelFakeIP(net.IPv4(198, 18, 0, 1)) || !isTunnelFakeIP(net.IPv4(198, 19, 255, 254)) {
		t.Fatal("expected 198.18.0.0/15 to be detected as TUN fake-IP")
	}
	if isTunnelFakeIP(net.IPv4(192, 168, 1, 185)) || isTunnelFakeIP(net.IPv4(198, 20, 0, 1)) {
		t.Fatal("non-TUN IP misclassified as TUN fake-IP")
	}
	if localIPPriority(net.IPv4(192, 168, 1, 10)) <= localIPPriority(net.IPv4(172, 16, 0, 1)) {
		t.Fatal("expected 192.168/16 to outrank 172.16/12")
	}
	if !isLikelyTunnelInterface("Mihomo") || !isLikelyTunnelInterface("Wintun Userspace Tunnel") || isLikelyTunnelInterface("Ethernet") {
		t.Fatal("unexpected tunnel interface classification")
	}
}
