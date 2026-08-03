//go:build windows

package enforce

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openConfigFilePlatform opens path relative to its volume or share root.
// os.Root uses handle-relative traversal on Windows, so a concurrently replaced
// reparse point cannot redirect the open outside that root. It also rejects
// reserved device names. Explicit Win32 device namespaces are refused first.
func openConfigFilePlatform(path string) (*os.File, error) {
	if hasWindowsSpecialNamespace(path) {
		return nil, fmt.Errorf("configuration path %q uses a Windows device, pipe, or mailslot namespace", path)
	}

	volume := filepath.VolumeName(path)
	if volume == "" {
		return nil, fmt.Errorf("configuration path %q has no Windows volume or share", path)
	}
	rootPath := volume + string(filepath.Separator)
	rel, err := filepath.Rel(rootPath, path)
	if err != nil {
		return nil, fmt.Errorf("resolving configuration path %q within %q: %w", path, rootPath, err)
	}
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("configuration path %q escapes its Windows volume or share", path)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("opening configuration root %q: %w", rootPath, err)
	}
	defer func() { _ = root.Close() }()

	// Overlapped I/O lets closing the returned file cancel a pending read. It
	// does not make the open asynchronous; root confinement above is what keeps
	// the open away from attacker-selected pipe and device namespaces.
	f, err := root.OpenFile(rel, os.O_RDONLY|windows.O_FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("opening configuration path %q: %w", path, err)
	}
	return f, nil
}

func hasWindowsSpecialNamespace(path string) bool {
	path = strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	if strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\\?\`) {
		return true
	}
	if !strings.HasPrefix(path, `\\`) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, `\\`), `\`)
	return len(parts) >= 2 && (parts[1] == "pipe" || parts[1] == "mailslot")
}
