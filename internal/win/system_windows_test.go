//go:build windows

package win

import (
	"crypto/sha256"
	"net"
	"os"
	"path/filepath"
	"testing"
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
}

func TestListProcesses(t *testing.T) {
	procs := listProcesses()
	if len(procs) < 10 {
		t.Fatalf("expected many processes, got %d", len(procs))
	}
	_ = SecuritySoftware()
}
