package enforce

import (
	"errors"
	"testing"
)

func TestOpenConfigFileRequiresAbsolutePath(t *testing.T) {
	f, err := openConfigFile("relative/mcp.json")
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, errConfigPathNotAbsolute) {
		t.Fatalf("relative open error = %v, want %v", err, errConfigPathNotAbsolute)
	}
}
