//go:build windows

package enforce

import (
	"strings"
	"testing"
)

func TestOpenConfigFileRejectsSpecialNamespaces(t *testing.T) {
	for _, path := range []string{
		`\\.\pipe\obot-sentry-test`,
		`\\?\GLOBALROOT\Device\NamedPipe\obot-sentry-test`,
		`\\localhost\pipe\obot-sentry-test`,
		`\\localhost\mailslot\obot-sentry-test`,
	} {
		t.Run(strings.ReplaceAll(path, `\`, "_"), func(t *testing.T) {
			f, err := openConfigFile(path)
			if f != nil {
				_ = f.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "namespace") {
				t.Fatalf("openConfigFile(%q) error = %v, want special-namespace rejection", path, err)
			}
		})
	}
}

func TestWindowsSpecialNamespaceAllowsOrdinaryUNCShare(t *testing.T) {
	path := `\\server\share\project\.cursor\mcp.json`
	if hasWindowsSpecialNamespace(path) {
		t.Fatalf("ordinary UNC path %q was classified as a special namespace", path)
	}
}

func TestNewEnvRejectsRelativeHome(t *testing.T) {
	t.Setenv("USERPROFILE", "relative-home")
	if _, err := NewEnv(); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("NewEnv error = %v, want relative-home rejection", err)
	}
}
