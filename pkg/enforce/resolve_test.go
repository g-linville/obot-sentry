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
