package enforce

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadJSONResults(t *testing.T) {
	f := newFixture(t, "darwin")

	var out map[string]any
	if got := loadJSON(f.path("missing.json"), &out); got != loadAbsent {
		t.Errorf("missing file = %v, want loadAbsent", got)
	}
	if got := loadJSON(f.write(f.path("bad.json"), `{`), &out); got != loadUnusable {
		t.Errorf("malformed file = %v, want loadUnusable", got)
	}
	if got := loadJSON(f.write(f.path("ok.json"), `{"a":1}`), &out); got != loadOK {
		t.Errorf("valid file = %v, want loadOK", got)
	}
	if got := loadJSON(f.mkdir(f.path("dir.json")), &out); got != loadUnusable {
		t.Errorf("directory = %v, want loadUnusable", got)
	}
}

func TestLoadJSONStripsBOM(t *testing.T) {
	f := newFixture(t, "darwin")
	path := f.write(f.path("bom.json"), "\xef\xbb\xbf"+`{"mcpServers":{"linear":{"url":"https://x.example.com/sse"}}}`)

	set, got := jsonServers(newConfigLoader(), path)(t.Context())
	if got != loadOK {
		t.Fatalf("load = %v, want loadOK", got)
	}
	if _, ok := set["linear"]; !ok {
		t.Fatal("the servers table did not decode")
	}
}

func TestLoadJSONRefusesOversizedFiles(t *testing.T) {
	f := newFixture(t, "darwin")
	path := f.write(f.path("huge.json"), `{"pad":"`+strings.Repeat("a", maxConfigBytes)+`"}`)

	var out map[string]any
	if got := loadJSON(path, &out); got != loadUnusable {
		t.Fatalf("oversized file = %v, want loadUnusable", got)
	}
}

func TestConfigLoaderUsesOneFileSnapshot(t *testing.T) {
	f := newFixture(t, "darwin")
	path := f.write(f.path("mcp.json"), `{"mcpServers":{"first":{"url":"https://first.example.com"}}}`)
	loader := newConfigLoader()
	load := jsonServers(loader, path)

	first, res := load(t.Context())
	if res != loadOK || first["first"].URL != "https://first.example.com" {
		t.Fatalf("first load = (%+v, %v)", first, res)
	}
	f.write(path, `{"mcpServers":{"second":{"url":"https://second.example.com"}}}`)
	second, res := load(t.Context())
	if res != loadOK {
		t.Fatalf("second load result = %v", res)
	}
	if second["first"].URL != "https://first.example.com" {
		t.Fatalf("cached snapshot changed after an on-disk edit: %+v", second)
	}
	if _, ok := second["second"]; ok {
		t.Fatalf("same invocation observed a later file version: %+v", second)
	}
}

func TestConfigLoaderCancellationDoesNotPoisonCache(t *testing.T) {
	f := newFixture(t, "darwin")
	path := f.write(f.path("mcp.json"), `{"mcpServers":{"server":{"url":"https://server.example.com"}}}`)
	load := jsonServers(newConfigLoader(), path)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, res := load(cancelled); res != loadUnusable {
		t.Fatalf("cancelled load = %v, want loadUnusable", res)
	}

	set, res := load(t.Context())
	if res != loadOK || set["server"].URL != "https://server.example.com" {
		t.Fatalf("later active load was poisoned by cancellation: (%+v, %v)", set, res)
	}
}

func TestEnvPathFallsBackToTheConventionalLocation(t *testing.T) {
	f := newFixture(t, "windows")

	got := f.Env.envPath("ProgramData", `C:\ProgramData`, "OpenAI", "Codex", "managed_config.toml")
	want := filepath.Join(f.Env.MachineRoot, `C:\ProgramData`, "OpenAI", "Codex", "managed_config.toml")
	if got != want {
		t.Fatalf("envPath = %q, want %q", got, want)
	}

	f.setenv("ProgramData", f.path("ProgramData"))
	got = f.Env.envPath("ProgramData", `C:\ProgramData`, "OpenAI", "Codex", "managed_config.toml")
	if want := f.path("ProgramData", "OpenAI", "Codex", "managed_config.toml"); got != want {
		t.Fatalf("envPath = %q, want %q", got, want)
	}
}

func TestMachinePathIsAbsoluteInProduction(t *testing.T) {
	env := Env{Home: "/Users/dev", GOOS: "darwin"}
	if got := env.machinePath(claudeManagedMCPDarwin); got != claudeManagedMCPDarwin {
		t.Fatalf("machinePath = %q, want %q", got, claudeManagedMCPDarwin)
	}
	if got := env.machinePath("/etc/codex/managed_config.toml"); got != "/etc/codex/managed_config.toml" {
		t.Fatalf("machinePath = %q", got)
	}
}
