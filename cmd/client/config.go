package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"civ6relay/internal/protocol"
)

const defaultRelayPort = 42056

// Config mirrors the Python client's config file so existing settings carry
// over unchanged.
type Config struct {
	RelayHost    string   `json:"relay_host"`
	RelayPort    int      `json:"relay_port"`
	DisplayName  string   `json:"display_name"`
	RelayHistory []string `json:"relay_history,omitempty"`
	HostPCAP     bool     `json:"host_pcap,omitempty"`
	HostP2P      bool     `json:"host_p2p,omitempty"`
}

const maxRelayHistory = 20

func configPath() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(base, "Civ6Relay", "config.json")
}

func loadConfig() Config {
	cfg := Config{RelayHost: "127.0.0.1", RelayPort: defaultRelayPort, DisplayName: defaultName()}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var stored Config
	if err := json.Unmarshal(data, &stored); err != nil {
		return cfg
	}
	if host := strings.TrimSpace(stored.RelayHost); host != "" {
		cfg.RelayHost = host
	}
	if stored.RelayPort >= 1 && stored.RelayPort <= 65535 {
		cfg.RelayPort = stored.RelayPort
	}
	if name, err := protocol.NormalizeName(stored.DisplayName); err == nil {
		cfg.DisplayName = name
	}
	cfg.RelayHistory = normalizeRelayHistory(stored.RelayHistory)
	cfg.HostPCAP = stored.HostPCAP
	cfg.HostP2P = stored.HostP2P
	return cfg
}

func normalizeRelayHistory(addresses []string) []string {
	var history []string
	seen := make(map[string]struct{}, min(len(addresses), maxRelayHistory))
	for _, address := range addresses {
		host, port, err := parseRelayAddress(address)
		if err != nil {
			continue
		}
		canonical := net.JoinHostPort(strings.ToLower(host), strconv.Itoa(port))
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		history = append(history, canonical)
		if len(history) == maxRelayHistory {
			break
		}
	}
	return history
}

func addRelayHistory(addresses []string, address string) []string {
	host, port, err := parseRelayAddress(address)
	if err != nil {
		return normalizeRelayHistory(addresses)
	}
	canonical := net.JoinHostPort(strings.ToLower(host), strconv.Itoa(port))
	history := normalizeRelayHistory(addresses)
	out := make([]string, 0, min(len(history)+1, maxRelayHistory))
	out = append(out, canonical)
	for _, existing := range history {
		if existing == canonical {
			continue
		}
		out = append(out, existing)
		if len(out) == maxRelayHistory {
			break
		}
	}
	return out
}

func saveConfig(cfg Config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeClientFile(path, data)
}

func writeClientFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".civ6relay-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp.Name(), path)
}

func defaultName() string {
	name := strings.TrimSpace(os.Getenv("USERNAME"))
	if name == "" {
		return "Player"
	}
	if utf8.RuneCountInString(name) > 24 {
		runes := []rune(name)
		name = string(runes[:24])
	}
	if _, err := protocol.NormalizeName(name); err != nil {
		return "Player"
	}
	return name
}

func parseRelayAddress(value string) (string, int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0, fmt.Errorf("中转地址不能为空")
	}
	if strings.Contains(value, ":") {
		host, portText, err := net.SplitHostPort(value)
		if err != nil {
			return "", 0, fmt.Errorf("中转地址格式应为 IP:端口，例如 203.0.113.10:42056")
		}
		host = strings.TrimSpace(host)
		if host == "" {
			return "", 0, fmt.Errorf("中转主机不能为空")
		}
		if strings.Contains(host, ":") {
			return "", 0, fmt.Errorf("中转服务器需要 IPv4 地址或域名")
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return "", 0, fmt.Errorf("中转端口必须是数字")
		}
		if port < 1 || port > 65535 {
			return "", 0, fmt.Errorf("中转端口必须在 1 到 65535 之间")
		}
		return host, port, nil
	}
	return value, defaultRelayPort, nil
}
