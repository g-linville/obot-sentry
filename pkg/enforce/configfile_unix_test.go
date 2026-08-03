//go:build darwin || linux

package enforce

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNewEnvRejectsRelativeHome(t *testing.T) {
	t.Setenv("HOME", "relative-home")
	if _, err := NewEnv(); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("NewEnv error = %v, want relative-home rejection", err)
	}
}

func TestConfigLoaderRefusesFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	result := make(chan loadResult, 1)
	go func() {
		_, res := newConfigLoader().readConfig(ctx, path)
		result <- res
	}()

	select {
	case res := <-result:
		if res != loadUnusable {
			t.Fatalf("FIFO load = %v, want loadUnusable", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}
