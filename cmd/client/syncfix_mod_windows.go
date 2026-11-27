//go:build windows

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

//go:embed civ6syncfix.zip
var embeddedSyncFixZip []byte

const (
	syncFixModFolderName = "Civ6SyncFix"
	civ6SteamAppID       = "289070"
	syncFixWorkshopID    = "3846151010"
	syncFixWorkshopURL   = "https://steamcommunity.com/sharedfiles/filedetails/?id=" + syncFixWorkshopID
)

type syncFixModStatus struct {
	ModsRoot          string
	InstallDir        string
	Installed         bool
	UpToDate          bool
	FileCount         int
	WorkshopDir       string
	WorkshopInstalled bool
	Err               string
}

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	ole32                    = syscall.NewLazyDLL("ole32.dll")
	procSHGetKnownFolderPath = shell32.NewProc("SHGetKnownFolderPath")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")

	// FOLDERID_Documents: {FDD39AD0-238F-46AF-ADB4-6C85480369C7}
	folderIDDocuments = guid{
		Data1: 0xFDD39AD0,
		Data2: 0x238F,
		Data3: 0x46AF,
		Data4: [8]byte{0xAD, 0xB4, 0x6C, 0x85, 0x48, 0x03, 0x69, 0xC7},
	}
)

// resolveDocumentsDir returns the user's real Windows Documents folder,
// respecting custom/redirected locations such as D:\...\文档.
func resolveDocumentsDir() (string, error) {
	var ptr *uint16
	r, _, _ := procSHGetKnownFolderPath.Call(
		uintptr(unsafe.Pointer(&folderIDDocuments)),
		0,
		0,
		uintptr(unsafe.Pointer(&ptr)),
	)
	if r == 0 && ptr != nil {
		defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(ptr)))
		if p := syscall.UTF16ToString((*[1 << 15]uint16)(unsafe.Pointer(ptr))[:]); p != "" {
			return filepath.Clean(p), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法定位文档目录: %w", err)
	}
	return filepath.Join(home, "Documents"), nil
}

func defaultCiv6ModsRoot() (string, error) {
	docs, err := resolveDocumentsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(docs, "My Games", "Sid Meier's Civilization VI", "Mods"), nil
}

func defaultSteamLibraryRoots() []string {
	var candidates []string
	addRoot := func(p string) {
		p = strings.TrimSpace(p)
		if p != "" {
			candidates = append(candidates, filepath.Clean(p))
		}
	}

	readRegPath := func(root registry.Key, subkey, valueName string, access uint32) {
		k, err := registry.OpenKey(root, subkey, access)
		if err != nil {
			return
		}
		defer k.Close()
		if val, _, err := k.GetStringValue(valueName); err == nil {
			addRoot(val)
		}
	}

	readRegPath(registry.CURRENT_USER, `Software\Valve\Steam`, "SteamPath", registry.QUERY_VALUE)
	readRegPath(registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Valve\Steam`, "InstallPath", registry.QUERY_VALUE)
	readRegPath(registry.LOCAL_MACHINE, `SOFTWARE\Valve\Steam`, "InstallPath", registry.QUERY_VALUE)
	for _, envName := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if base := strings.TrimSpace(os.Getenv(envName)); base != "" {
			addRoot(filepath.Join(base, "Steam"))
		}
	}

	seen := make(map[string]struct{})
	var roots []string
	recordUnique := func(dir string) {
		cleaned := filepath.Clean(strings.TrimSpace(dir))
		if cleaned == "" || cleaned == "." {
			return
		}
		key := strings.ToLower(cleaned)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		roots = append(roots, cleaned)
	}

	for _, steamDir := range candidates {
		recordUnique(steamDir)
		for _, vdfRel := range []string{
			filepath.Join("steamapps", "libraryfolders.vdf"),
			filepath.Join("config", "libraryfolders.vdf"),
		} {
			raw, err := os.ReadFile(filepath.Join(steamDir, vdfRel))
			if err != nil {
				continue
			}
			for _, libPath := range parseLibraryFoldersVDF(string(raw)) {
				recordUnique(libPath)
			}
		}
	}
	return roots
}

func parseLibraryFoldersVDF(content string) []string {
	var libs []string
	var prevToken string
	expectValue := false

	i := 0
	for i < len(content) {
		ch := content[i]
		if ch == '"' {
			i++
			var b strings.Builder
			for i < len(content) {
				c := content[i]
				if c == '\\' && i+1 < len(content) {
					next := content[i+1]
					if next == '\\' || next == '"' {
						b.WriteByte(next)
						i += 2
						continue
					}
				}
				if c == '"' {
					i++
					break
				}
				b.WriteByte(c)
				i++
			}
			tok := b.String()
			if expectValue {
				if strings.EqualFold(prevToken, "path") || isAllDigits(prevToken) {
					if cleaned := filepath.Clean(strings.TrimSpace(tok)); cleaned != "" && cleaned != "." {
						libs = append(libs, cleaned)
					}
				}
				expectValue = false
			} else {
				prevToken = tok
				expectValue = true
			}
			continue
		}
		if ch == '{' || ch == '}' {
			expectValue = false
			i++
			continue
		}
		if ch == '/' && i+1 < len(content) && content[i+1] == '/' {
			for i < len(content) && content[i] != '\n' {
				i++
			}
			continue
		}
		i++
	}
	return libs
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func findWorkshopModDir(libraryRoots []string, appID, itemID string) (string, bool) {
	for _, root := range libraryRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		dir := filepath.Join(filepath.Clean(root), "steamapps", "workshop", "content", appID, itemID)
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) == 0 {
			continue
		}
		return dir, true
	}
	return "", false
}

type embeddedModEntry struct {
	relPath string
	data    []byte
	hash    [32]byte
}

func loadEmbeddedSyncFixEntries() ([]embeddedModEntry, error) {
	if len(embeddedSyncFixZip) == 0 {
		return nil, errors.New("内置同步修复 Mod 资源为空")
	}
	zr, err := zip.NewReader(bytes.NewReader(embeddedSyncFixZip), int64(len(embeddedSyncFixZip)))
	if err != nil {
		return nil, fmt.Errorf("解析内置同步修复 Mod 压缩包失败: %w", err)
	}
	var entries []embeddedModEntry
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if f.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			continue
		}
		name = strings.TrimPrefix(name, "/")
		// Normalize both "Civ6SyncFix/..." and bare relative paths.
		if strings.HasPrefix(name, syncFixModFolderName+"/") {
			name = strings.TrimPrefix(name, syncFixModFolderName+"/")
		}
		cleaned := filepath.Clean(filepath.FromSlash(name))
		if cleaned == "." || cleaned == "" || filepath.IsAbs(cleaned) ||
			cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("非法压缩包路径: %q", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("读取内置文件 %s 失败: %w", f.Name, err)
		}
		data, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("读取内置文件 %s 失败: %w", f.Name, readErr)
		}
		entries = append(entries, embeddedModEntry{
			relPath: cleaned,
			data:    data,
			hash:    sha256.Sum256(data),
		})
	}
	if len(entries) == 0 {
		return nil, errors.New("内置同步修复 Mod 压缩包内无文件")
	}
	return entries, nil
}

func inspectSyncFixMod(modsRoot string, libraryRoots []string) syncFixModStatus {
	st := syncFixModStatus{
		ModsRoot:   filepath.Clean(modsRoot),
		InstallDir: filepath.Join(filepath.Clean(modsRoot), syncFixModFolderName),
	}
	if workshopDir, ok := findWorkshopModDir(libraryRoots, civ6SteamAppID, syncFixWorkshopID); ok {
		st.WorkshopDir = workshopDir
		st.WorkshopInstalled = true
	}
	entries, err := loadEmbeddedSyncFixEntries()
	if err != nil {
		st.Err = err.Error()
		return st
	}
	st.FileCount = len(entries)

	allPresent := true
	allMatch := true
	for _, e := range entries {
		dst := filepath.Join(st.InstallDir, e.relPath)
		raw, err := os.ReadFile(dst)
		if err != nil {
			allPresent = false
			allMatch = false
			break
		}
		if sha256.Sum256(raw) != e.hash {
			allMatch = false
		}
	}
	st.Installed = allPresent
	st.UpToDate = allPresent && allMatch
	return st
}

func installSyncFixMod(modsRoot string) (string, error) {
	if strings.TrimSpace(modsRoot) == "" {
		return "", errors.New("目标模组目录为空")
	}
	entries, err := loadEmbeddedSyncFixEntries()
	if err != nil {
		return "", err
	}
	installDir := filepath.Join(filepath.Clean(modsRoot), syncFixModFolderName)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return "", fmt.Errorf("创建模组目录失败: %w", err)
	}
	for _, e := range entries {
		dst := filepath.Join(installDir, e.relPath)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", fmt.Errorf("创建子目录失败 (%s): %w", e.relPath, err)
		}
		if err := os.WriteFile(dst, e.data, 0o644); err != nil {
			return "", fmt.Errorf("写入文件 %s 失败（若文明 VI 正在运行请先退出游戏）: %w", e.relPath, err)
		}
	}
	st := inspectSyncFixMod(modsRoot, nil)
	if !st.UpToDate {
		return "", errors.New("安装后校验失败，文件未完整写入")
	}
	return installDir, nil
}

func uninstallSyncFixMod(modsRoot string) error {
	if strings.TrimSpace(modsRoot) == "" {
		return errors.New("目标模组目录为空")
	}
	installDir := filepath.Join(filepath.Clean(modsRoot), syncFixModFolderName)
	if _, err := os.Stat(installDir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.RemoveAll(installDir); err != nil {
		return fmt.Errorf("卸载失败（若文明 VI 正在运行请先退出游戏）: %w", err)
	}
	return nil
}
