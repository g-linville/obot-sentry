package enforce

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Claude Code configuration file locations. Linux is deliberately absent
// throughout: obot-sentry builds for darwin and windows only, and
// hookinstall.supportedPlatform rejects everything else.
const (
	claudeManagedMCPDarwin = "/Library/Application Support/ClaudeCode/managed-mcp.json"
	// windowsProgramFilesDefault stands in for %PROGRAMFILES% when it is unset,
	// which happens when a test models the Windows layout from another OS.
	windowsProgramFilesDefault = `C:\Program Files`
	// claudeAIConnectorPrefix namespaces a claude.ai account connector's tools.
	claudeAIConnectorPrefix = "claude_ai_"
)

// claudeJSON is the part of ~/.claude.json the resolver reads.
type claudeJSON struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	} `json:"projects"`
	// ClaudeAIMCPEverConnected lists the display names of claude.ai account
	// connectors this installation has connected to. It is the only local evidence
	// such a connector exists.
	ClaudeAIMCPEverConnected []string `json:"claudeAiMcpEverConnected"`
}

type cachedClaudeJSON struct {
	doc claudeJSON
	res loadResult
}

func (l *configLoader) loadClaudeJSON(ctx context.Context, path string) (claudeJSON, loadResult) {
	if ctx.Err() != nil {
		return claudeJSON{}, loadUnusable
	}
	if cached, ok := l.claudeDocs[path]; ok {
		return cached.doc, cached.res
	}
	var doc claudeJSON
	res := l.loadJSON(ctx, path, &doc)
	if ctx.Err() != nil {
		return claudeJSON{}, loadUnusable
	}
	l.claudeDocs[path] = cachedClaudeJSON{doc: doc, res: res}
	return doc, res
}

// managedMCPPath returns the enterprise managed MCP configuration path.
func (e Env) managedMCPPath() string {
	if e.windows() {
		return e.envPath("PROGRAMFILES", windowsProgramFilesDefault, "ClaudeCode", "managed-mcp.json")
	}
	return e.machinePath(claudeManagedMCPDarwin)
}

// resolveClaudeCode resolves a Claude Code server name against its MCP
// configuration, then against the claude.ai account connectors.
func resolveClaudeCode(ctx context.Context, loader *configLoader, env Env, req ResolveRequest, serverName string, tr *tracer) Resolution {
	claudePath := env.homePath(".claude.json")
	claude, claudeRes := loader.loadClaudeJSON(ctx, claudePath)

	// Most Claude Code keys survive namespacing untouched — hyphens and underscores
	// are legal in a tool namespace — so most lookups here match exactly. A key
	// holding anything else does not: "my.server" and "@acme/server" are namespaced
	// as "my_server" and "_acme_server", so an exact-only lookup would miss every
	// scope and deny a correctly configured server. formClaudeCode is what closes
	// that.
	names := lookup{names: []string{serverName}, form: formClaudeCode}

	scopes, gap := claudeCodeScopes(ctx, loader, env, req.CWD, serverName, claudePath, claude, claudeRes)
	m, out := resolveScopes(ctx, scopes, names, tr)
	switch out {
	case outcomeAmbiguous:
		return ambiguous(req.Agent, serverName)
	case outcomeClosed:
		// The managed lockdown stops plugin servers from running at all, so an
		// incompletely enumerated plugin tree cannot change this answer and is not
		// worth reporting over it.
		return notFound(req.Agent, serverName, fmt.Sprintf(
			"MCP server %q is not in Claude Code's managed MCP configuration, which cannot be overridden", serverName))
	}

	// A plugin source we could not enumerate denies whatever the ladder concluded,
	// including a match: with the plugin set unknown, we cannot say the entry we found
	// is the only one folding to this namespace. See pluginGap.
	if gap != nil {
		return unresolvedPluginGap(serverName, gap, tr)
	}

	if out == outcomeFound {
		return resolved(env, m.key, m.entry)
	}

	if connector, ok := resolveClaudeAIConnector(claude, claudeRes, claudePath, serverName, tr); ok {
		// The display name, not the tool-name hint that found it: the hint is the
		// namespace form (claude_ai_Linear for a connector listed as "claude.ai
		// Linear"), which appears in no allowlist entry an administrator could write.
		res := Resolution{ServerName: connector}
		res.Identity.Connector = connector
		return res
	}

	return notFound(req.Agent, serverName, fmt.Sprintf(
		"MCP server %q was not found in any Claude Code MCP configuration", serverName))
}

// claudeCodeScopes returns the Claude Code MCP configuration scopes, highest
// precedence first: the managed config, then the project-scoped sources, then the
// global servers table, then the installed plugins.
// It also reports a plugin source that exists and could not be enumerated, which
// denies rather than falling through — see pluginGap.
func claudeCodeScopes(ctx context.Context, loader *configLoader, env Env, cwd, serverName, claudePath string, claude claudeJSON, claudeRes loadResult) ([]scope, *pluginGap) {
	managedPath := env.managedMCPPath()
	// Claude Code documents the managed server set as something users cannot
	// override, so a managed file that exists and does not list the server ends
	// resolution rather than falling through to user configuration.
	//
	// That exclusivity reaches plugin servers too, and not by inference: the agent
	// returns the managed set before plugin discovery runs at all, and the one
	// documented escape from the lockdown covers claude.ai connectors only. A
	// managed file therefore ends a plugin call here as well, with the same reason.
	scopes := []scope{{
		path:   managedPath,
		key:    mcpServersKey,
		closed: true,
		load:   jsonServers(loader, managedPath),
	}}

	// projectScopes hands out one consecutive rank per scope from the one it is
	// given, so the global table sits just below the last of them.
	project := projectScopes(loader, env, cwd, claudePath, claude, claudeRes, 1)
	scopes = append(scopes, project...)

	globalRank := 1 + len(project)
	scopes = append(scopes, scope{
		path: claudePath,
		key:  mcpServersKey,
		rank: globalRank,
		load: fixedServers(decodeServers(claude.MCPServers), claudeRes),
	})

	// Plugins rank last. The agent merges their servers first and lets user, project,
	// and local configuration overwrite them by key, and nothing reserves the plugin
	// namespace against a user writing one of its names by hand — so a name declared
	// in both places is the user's, not the plugin's.
	plugin, gap := claudePluginScopes(ctx, loader, env, cwd, serverName, globalRank+1)
	return append(scopes, plugin...), gap
}

// maxProjectDepth bounds the ancestor walk.
const maxProjectDepth = 40

// projectScopes returns the project-scoped Claude Code sources that may govern a
// call made from cwd, most specific first.
//
// Claude Code keys projects{} by the directory it was launched in, which is
// frequently not a repository root, and reads .mcp.json from its project root.
// Both are found by walking cwd's ancestors: within one directory the project
// file outranks the projects{} entry, and a deeper directory outranks a shallower
// one. Only the nearest .mcp.json is a scope, because only one project root is
// ever live for a session.
func projectScopes(loader *configLoader, env Env, cwd, claudePath string, claude claudeJSON, claudeRes loadResult, rank int) []scope {
	dirs := ancestors(cwd)
	if len(dirs) == 0 {
		return nil
	}

	keyByDir := make(map[string]string, len(claude.Projects))
	for key := range claude.Projects {
		dir := env.comparableDir(key)
		if dir == "" {
			continue
		}
		// Two keys can name one directory through spelling alone. Taking the lowest
		// keeps resolution stable across runs, since map order is not.
		if existing, ok := keyByDir[dir]; !ok || key < existing {
			keyByDir[dir] = key
		}
	}

	projectFileDir := nearestProjectFileDir(dirs)
	scopes := make([]scope, 0, 4)
	for _, dir := range dirs {
		if dir == projectFileDir {
			path := filepath.Join(dir, ".mcp.json")
			scopes = append(scopes, scope{
				path: path,
				key:  mcpServersKey,
				rank: rank,
				load: jsonServers(loader, path),
			})
			rank++
		}
		key, ok := keyByDir[env.comparableDir(dir)]
		if !ok {
			continue
		}
		scopes = append(scopes, scope{
			path: claudePath,
			key:  fmt.Sprintf("projects[%q].%s", key, mcpServersKey),
			rank: rank,
			load: fixedServers(decodeServers(claude.Projects[key].MCPServers), claudeRes),
		})
		rank++
	}
	return scopes
}

// nearestProjectFileDir returns the first directory in dirs holding a .mcp.json.
// With none, it returns dirs[0] anyway, so that the file an operator expects to
// be read is named in the trace as absent rather than going unmentioned.
func nearestProjectFileDir(dirs []string) string {
	for _, dir := range dirs {
		info, err := os.Lstat(filepath.Join(dir, ".mcp.json"))
		if err == nil && !info.IsDir() {
			return dir
		}
	}
	return dirs[0]
}

// ancestors returns an absolute cwd and each directory above it, nearest first.
// A relative cwd cannot identify a stable configuration scope and is ignored.
func ancestors(cwd string) []string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return nil
	}
	dir := filepath.Clean(cwd)
	dirs := make([]string, 0, 8)
	for range maxProjectDepth {
		dirs = append(dirs, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dirs
}

// comparableDir is the form of a directory path used to decide whether two paths
// name the same directory.
func (e Env) comparableDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if e.windows() {
		return strings.ToLower(path)
	}
	return path
}

func resolveClaudeAIConnector(claude claudeJSON, claudeRes loadResult, claudePath, serverName string, tr *tracer) (string, bool) {
	if !strings.HasPrefix(serverName, claudeAIConnectorPrefix) {
		return "", false
	}
	if claudeRes != loadOK || len(claude.ClaudeAIMCPEverConnected) == 0 {
		tr.miss(claudePath, "claudeAiMcpEverConnected", claudeRes)
		return "", false
	}

	connected := make(map[string]struct{}, len(claude.ClaudeAIMCPEverConnected))
	for _, name := range claude.ClaudeAIMCPEverConnected {
		connected[name] = struct{}{}
	}

	// The stored entries are display names ("claude.ai MyServer"), which are never the
	// namespace form the call reports, so this always resolves through the form
	// rather than exactly.
	//
	// That is also what handles a duplicate display name, with no suffix rule of its
	// own. Claude Code disambiguates by renaming the second connector to
	// "claude.ai MyServer (2)", and formClaudeCode of that is "claude_ai_MyServer_2" —
	// so the reported name matches the stored entry outright. Stripping a trailing
	// "_<n>" off the reported name and looking for "claude.ai MyServer" instead, which
	// is the obvious-looking thing to do, finds the FIRST connector: the wrong one.
	names := lookup{names: []string{serverName}, form: formClaudeCode}
	if _, connector, ok := matchName(names, connected); ok {
		tr.hit(claudePath, "claudeAiMcpEverConnected", "connector "+connector)
		return connector, true
	}
	tr.miss(claudePath, "claudeAiMcpEverConnected", claudeRes)
	return "", false
}
