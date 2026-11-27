package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAddRelayHistoryKeepsRecentUniqueValidAddresses(t *testing.T) {
	history := addRelayHistory([]string{
		"relay.example:42056",
		"RELAY.example:42056",
		"bad host:invalid",
		"192.0.2.2",
	}, "Relay.Example:42056")
	want := []string{"relay.example:42056", "192.0.2.2:42056"}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("got %#v, want %#v", history, want)
	}
}

func TestAddRelayHistoryMovesAddressToFrontAndCapsLength(t *testing.T) {
	history := make([]string, maxRelayHistory+2)
	for index := range history {
		history[index] = fmt.Sprintf("relay-%d.example:42056", index)
	}
	history = addRelayHistory(history, "relay-1.example:42056")
	if len(history) != maxRelayHistory || history[0] != "relay-1.example:42056" || history[1] != "relay-0.example:42056" {
		t.Fatalf("unexpected recent relay ordering: %#v", history)
	}
}

func TestHostPCAPDefaultsOffAndPersists(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())

	cfg := loadConfig()
	if cfg.HostPCAP {
		t.Fatalf("expected HostPCAP to default to false")
	}

	legacy := []byte(`{"relay_host":"192.0.2.10","relay_port":42056,"display_name":"Player"}`)
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg = loadConfig()
	if cfg.HostPCAP {
		t.Fatalf("expected legacy config without host_pcap to default to false")
	}

	cfg.HostPCAP = true
	cfg.HostP2P = true
	cfg.MaxMembers = 12
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reloaded := loadConfig()
	if !reloaded.HostPCAP {
		t.Fatalf("expected saved HostPCAP=true to persist")
	}
	if !reloaded.HostP2P {
		t.Fatalf("expected saved HostP2P=true to persist")
	}
	if reloaded.MaxMembers != 12 {
		t.Fatalf("expected saved MaxMembers=12 to persist, got %d", reloaded.MaxMembers)
	}
}

func TestParseMaxMembers(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"  ", 0, false},
		{"0", 0, false},
		{"无限", 0, false},
		{"不限", 0, false},
		{"8", 8, false},
		{"253", 253, false},
		{"-1", 0, true},
		{"254", 0, true},
		{"abc", 0, true},
	} {
		got, err := parseMaxMembers(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("parseMaxMembers(%q): err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
		if !tc.wantErr && got != tc.want {
			t.Fatalf("parseMaxMembers(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
