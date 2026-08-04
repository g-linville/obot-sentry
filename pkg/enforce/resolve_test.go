package enforce

import (
	"context"
	"strings"
	"testing"

	"github.com/obot-platform/obot-sentry/pkg/localagent"
	"github.com/obot-platform/obot/apiclient/types"
)

// TestResolutionString covers the human-readable rendering of each identity form,
// which the resolve diagnostic prints and the deny copy names.
func TestResolutionString(t *testing.T) {
	cases := []struct {
		name string
		res  Resolution
		want string
	}{
		{"url", Resolution{Identity: types.EnforcementDecisionServer{URL: "https://mcp.linear.app/sse"}}, "https://mcp.linear.app/sse"},
		{
			name: "package with a version",
			res:  Resolution{Identity: types.EnforcementDecisionServer{Package: &types.AllowlistServerPackage{Source: "npm", Name: "linear-mcp", Version: "1.2.3"}, Command: "npx"}},
			want: "npm / linear-mcp / 1.2.3",
		},
		{
			name: "package with no version pin",
			res:  Resolution{Identity: types.EnforcementDecisionServer{Package: &types.AllowlistServerPackage{Source: "pypi", Name: "mcp-server-git"}, Command: "uvx"}},
			want: "pypi / mcp-server-git / any version",
		},
		{"connector", Resolution{Identity: types.EnforcementDecisionServer{Connector: "claude.ai Linear"}}, "connector claude.ai Linear"},
		{"unresolved runner", Resolution{Identity: types.EnforcementDecisionServer{Command: "node"}}, "node"},
		{"built-in server", Resolution{ServerName: "Claude_Preview"}, ""},
	}

	for _, tc := range cases {
		if got := tc.res.String(); got != tc.want {
			t.Errorf("%s: String() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEnforcementURLKeepsOnlyMatchingComponents(t *testing.T) {
	raw := "https://user:token-secret@example.com:8443/mcp/path?api_key=query-secret#fragment-secret"
	want := "https://example.com:8443/mcp/path"
	if got, ok := enforcementURL(raw); !ok || got != want {
		t.Fatalf("enforcementURL(%q) = %q, want %q", raw, got, want)
	}

	res := resolved(Env{GOOS: "darwin"}, "server", mcpEntry{URL: raw})
	if res.Identity.URL != want {
		t.Fatalf("resolved URL = %q, want %q", res.Identity.URL, want)
	}
	for _, secret := range []string{"user", "token-secret", "api_key", "query-secret", "fragment-secret"} {
		if got := string(mustJSON(res.Identity)); strings.Contains(got, secret) {
			t.Fatalf("decision identity leaked %q: %s", secret, got)
		}
	}
}

func TestEnforcementURLRejectsSecretBearingInvalidForms(t *testing.T) {
	for _, raw := range []string{
		"https:token-secret@example.com/mcp",
		"https://example.com/%zz?token-secret",
		"example.com/mcp?token-secret",
	} {
		if got, ok := enforcementURL(raw); ok || got != "" {
			t.Errorf("enforcementURL(%q) = (%q, %v), want rejected with no forwarded text", raw, got, ok)
		}
		res := resolved(Env{GOOS: "darwin"}, "server", mcpEntry{URL: raw})
		if !res.Unresolved || res.Identity.URL != "" {
			t.Errorf("resolved(%q) = %+v, want an unresolved identity", raw, res)
		}
		if strings.Contains(string(mustJSON(res)), "token-secret") {
			t.Errorf("unresolved result leaked URL text: %+v", res)
		}
	}
}

func TestPackageEnvironmentThatChangesExecutionIsUnresolved(t *testing.T) {
	cases := []struct {
		name    string
		command string
		args    []string
		env     map[string]string
		wantVar string
	}{
		{
			name:    "npx registry",
			command: "npx",
			args: []string{
				"allowed@1.2.3",
			},
			env: map[string]string{
				"NPM_CONFIG_REGISTRY": "https://attacker.invalid",
			},
			wantVar: "NPM_CONFIG_REGISTRY",
		},
		{
			name:    "npx code injection",
			command: "npx",
			args: []string{
				"allowed@1.2.3",
			},
			env: map[string]string{
				"NODE_OPTIONS": "--require=/tmp/attacker.js",
			},
			wantVar: "NODE_OPTIONS",
		},
		{
			name:    "npx runner replacement",
			command: "npx",
			args: []string{
				"allowed@1.2.3",
			},
			env: map[string]string{
				"PATH": "/tmp/attacker-bin",
			},
			wantVar: "PATH",
		},
		{
			name:    "uvx index",
			command: "uvx",
			args: []string{
				"allowed@1.2.3",
			},
			env: map[string]string{
				"UV_INDEX_URL": "https://attacker.invalid/simple",
			},
			wantVar: "UV_INDEX_URL",
		},
		{
			name:    "uvx code injection",
			command: "uvx",
			args: []string{
				"allowed@1.2.3",
			},
			env: map[string]string{
				"PYTHONPATH": "/tmp/attacker-python",
			},
			wantVar: "PYTHONPATH",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := resolved(Env{GOOS: "darwin"}, "server", mcpEntry{
				Command:     tc.command,
				Args:        tc.args,
				Environment: tc.env,
			})
			assertUnresolved(t, res, tc.wantVar)
			if strings.Contains(res.Reason, "attacker") {
				t.Fatalf("unresolved reason leaked an environment value: %q", res.Reason)
			}
		})
	}
}

func TestInheritedPackageEnvironmentThatChangesExecutionIsUnresolved(t *testing.T) {
	cases := []struct {
		name    string
		command string
		args    []string
		environ []string
		wantVar string
	}{
		{
			name:    "npx registry",
			command: "npx",
			args: []string{
				"allowed",
			},
			environ: []string{
				"NPM_CONFIG_REGISTRY=https://attacker.invalid",
			},
			wantVar: "NPM_CONFIG_REGISTRY",
		},
		{
			name:    "npx code injection",
			command: "npx",
			args: []string{
				"allowed",
			},
			environ: []string{
				"NODE_OPTIONS=--require=/tmp/attacker.js",
			},
			wantVar: "NODE_OPTIONS",
		},
		{
			name:    "uvx index",
			command: "uvx",
			args: []string{
				"allowed",
			},
			environ: []string{
				"UV_INDEX_URL=https://attacker.invalid/simple",
			},
			wantVar: "UV_INDEX_URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := resolved(Env{
				GOOS:    "darwin",
				Environ: func() []string { return tc.environ },
			}, "server", mcpEntry{
				Command: tc.command,
				Args:    tc.args,
			})
			assertUnresolved(t, res, tc.wantVar)
		})
	}
}

func TestPackageEnvironmentAllowsSecretsAndHomebrewPath(t *testing.T) {
	res := resolved(Env{
		GOOS: "darwin",
		Environ: func() []string {
			return []string{
				"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin",
				"NPM_CONFIG_PREFIX=/opt/homebrew",
				"NODE_EXTRA_CA_CERTS=/managed/corporate-ca.pem",
			}
		},
	}, "server", mcpEntry{
		Command:     "npx",
		Args:        []string{"allowed@1.2.3"},
		Environment: map[string]string{"GITHUB_TOKEN": "secret-value"},
	})
	if res.Unresolved {
		t.Fatalf("ordinary secret environment and Homebrew path were rejected: %s", res.Reason)
	}
	if res.Identity.Package == nil || res.Identity.Package.Name != "allowed" {
		t.Fatalf("package = %+v, want npm/allowed", res.Identity.Package)
	}
	if strings.Contains(string(mustJSON(res)), "secret-value") {
		t.Fatal("the package resolution leaked a configured secret")
	}
}

func TestResolveContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res := ResolveContext(ctx, Env{Home: t.TempDir(), GOOS: "darwin"}, ResolveRequest{
		Agent:      localagent.Codex,
		ServerName: "server",
	})
	assertUnresolved(t, res, "resolution was cancelled")
}

func TestBuiltinNamesMatchTheNamespaceForm(t *testing.T) {
	for _, name := range []string{
		"Claude_Preview", // "Claude Preview", space folded
		"Claude_Browser", // "Claude Browser", space folded
		"claude-in-chrome",
		"computer-use",
		"workspace",
	} {
		if !isBuiltinAgentMCP(localagent.ClaudeCode, name) {
			t.Errorf("isBuiltinAgentMCP(claude-code, %q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"", "linear", "claude", "preview", "workspaces",
		"claude_preview",   // lowercased: Claude Code preserves case
		"ClaudePreview",    // separator deleted: Claude Code folds, never deletes
		"claudeinchrome",   // same
		"claude_in_chrome", // hyphens are legal, so they survive rather than folding
	} {
		if isBuiltinAgentMCP(localagent.ClaudeCode, name) {
			t.Errorf("isBuiltinAgentMCP(claude-code, %q) = true, want false", name)
		}
	}
}

func TestBuiltinNamesUseTheAgentsOwnForm(t *testing.T) {
	for _, name := range []string{"Claude_Preview", "workspace", "computer-use"} {
		if isBuiltinAgentMCP(localagent.Codex, name) {
			t.Errorf("isBuiltinAgentMCP(codex, %q) = true, want false", name)
		}
	}
}
