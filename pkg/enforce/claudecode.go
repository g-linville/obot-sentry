package enforce

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Claude Code configuration file locations. Linux is deliberately absent
// throughout: obot-sentry builds for darwin and windows only, and
// hookinstall.supportedPlatform rejects everything else.
//
// Both machine-scoped paths are spelled slash-separated so machinePath can convert
// them, and neither is looked up in the environment. That is not a simplification:
// the agent compiles one fixed location per platform in and never consults
// %PROGRAMFILES%, so reading it here would point at a file the agent does not read.
const (
	claudeManagedMCPDarwin  = "/Library/Application Support/ClaudeCode/managed-mcp.json"
	claudeManagedMCPWindows = "C:/Program Files/ClaudeCode/managed-mcp.json"
	// claudeAIConnectorPrefix namespaces a claude.ai account connector's tools.
	claudeAIConnectorPrefix = "claude_ai_"
	// claudeProjectMCPFile is the project-scoped server file, read from the working
	// directory and from every directory above it.
	claudeProjectMCPFile = ".mcp.json"
)

// Environment variables that relocate Claude Code's own state.
//
// Each is honored only when it holds an absolute path. A relative one cannot be
// resolved from here, because it would be taken against the agent's working
// directory rather than the hook's, and guessing wrong would point every lookup at
// a tree that does not exist.
const (
	claudeConfigDirEnv      = "CLAUDE_CONFIG_DIR"
	claudePluginCacheDirEnv = "CLAUDE_CODE_PLUGIN_CACHE_DIR"
	claudePluginSeedDirEnv  = "CLAUDE_CODE_PLUGIN_SEED_DIR"
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
		return e.machinePath(claudeManagedMCPWindows)
	}
	return e.machinePath(claudeManagedMCPDarwin)
}

// envDir returns the absolute directory held by an environment variable, or empty.
func (e Env) envDir(key string) string {
	dir := strings.TrimSpace(e.getenv(key))
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Clean(dir)
}

// claudeConfigDir is the directory Claude Code keeps its per-user state in: the
// installed plugins, the skills tree, and the user settings file. An override in
// the environment relocates the whole tree.
func (e Env) claudeConfigDir() string {
	if dir := e.envDir(claudeConfigDirEnv); dir != "" {
		return dir
	}
	return e.homePath(".claude")
}

// claudeJSONPath is the file holding Claude Code's user-scoped server table and its
// per-project tables.
//
// Two things make this more than a fixed name. An alternate file inside the config
// directory takes precedence whenever it exists, so a machine carrying one is
// reading a different file entirely. And the config directory override moves this
// file too — but to a sibling of that directory rather than inside it, which is why
// the override is applied here directly instead of through claudeConfigDir.
func (e Env) claudeJSONPath() string {
	if alt := filepath.Join(e.claudeConfigDir(), ".config.json"); existsAsFile(alt) {
		return alt
	}
	if dir := e.envDir(claudeConfigDirEnv); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return e.homePath(".claude.json")
}

// claudePluginsDir is the root of the installed-plugin tree. It has an override of
// its own, which wins over the config directory.
func (e Env) claudePluginsDir() string {
	if dir := e.envDir(claudePluginCacheDirEnv); dir != "" {
		return dir
	}
	return filepath.Join(e.claudeConfigDir(), "plugins")
}

// claudeSkillsDir is the user-scoped skills root.
func (e Env) claudeSkillsDir() string {
	return filepath.Join(e.claudeConfigDir(), "skills")
}

// claudePluginSeedDirs are the seeded plugin trees named by the environment as a
// list in the platform's path-list form. Each holds the same cache layout as the
// main plugin tree and is consulted when an installation records no location of its
// own — see pluginCachePaths.
func (e Env) claudePluginSeedDirs() []string {
	raw := strings.TrimSpace(e.getenv(claudePluginSeedDirEnv))
	if raw == "" {
		return nil
	}
	var out []string
	for _, dir := range filepath.SplitList(raw) {
		if dir = strings.TrimSpace(dir); dir != "" && filepath.IsAbs(dir) {
			out = append(out, filepath.Clean(dir))
		}
	}
	return out
}

// existsAsFile reports whether path is present and is not a directory. It is a
// presence probe rather than a read: the caller only needs to know whether the file
// is there before deciding to name it as a source.
func existsAsFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && !info.IsDir()
}

// resolveClaudeCode resolves a Claude Code server name against its MCP
// configuration, then against the claude.ai account connectors.
func resolveClaudeCode(ctx context.Context, loader *configLoader, env Env, req ResolveRequest, serverName string, tr *tracer) Resolution {
	claudePath := env.claudeJSONPath()
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
	if out == outcomeClosed {
		// The managed lockdown stops plugin servers from running at all, so an
		// incompletely enumerated plugin tree cannot change this answer and is not
		// worth reporting over it.
		//
		// The claude.ai account connectors are the one exception, and they are not
		// suppressed unconditionally: an administrator can opt back into them
		// alongside the managed configuration. That opt-in lives in managed settings
		// this hook cannot read in full — some of it is outside any file it could
		// parse — so a name that matches a connector this installation has connected
		// to is reported as undecidable rather than guessed in either direction.
		connectors := resolveClaudeAIConnectors(claude, claudeRes, claudePath, serverName, tr)
		if len(connectors) > 1 {
			return ambiguous(req.Agent, serverName)
		}
		if len(connectors) == 1 {
			return unresolved(connectors[0], fmt.Sprintf(
				"MCP server %q is not in Claude Code's managed MCP configuration, but it names a claude.ai connector this installation has connected to, and whether the managed configuration suppresses that connector depends on a managed setting the hook cannot read",
				serverName))
		}
		return notFound(req.Agent, serverName, fmt.Sprintf(
			"MCP server %q is not in Claude Code's managed MCP configuration, which cannot be overridden", serverName))
	}
	if out == outcomeAmbiguous {
		// Include connector matches in the trace even though the configuration
		// declarations already make the result ambiguous.
		resolveClaudeAIConnectors(claude, claudeRes, claudePath, serverName, tr)
		return ambiguous(req.Agent, serverName)
	}

	// A plugin source we could not enumerate denies whatever the ladder concluded,
	// including a match: with the plugin set unknown, we cannot say the entry we found
	// is the only one folding to this namespace. See pluginGap.
	if gap != nil {
		return unresolvedPluginGap(serverName, gap, tr)
	}

	connectors := resolveClaudeAIConnectors(claude, claudeRes, claudePath, serverName, tr)
	if len(connectors) > 1 || (out == outcomeFound && len(connectors) == 1) {
		return ambiguous(req.Agent, serverName)
	}
	if out == outcomeFound {
		return resolved(env, m.key, m.entry)
	}
	if len(connectors) == 1 {
		// The display name, not the tool-name hint that found it: the hint is the
		// namespace form (claude_ai_Linear for a connector listed as "claude.ai
		// Linear"), which appears in no allowlist entry an administrator could write.
		res := Resolution{ServerName: connectors[0]}
		res.Identity.Connector = connectors[0]
		return res
	}

	return notFound(req.Agent, serverName, fmt.Sprintf(
		"MCP server %q was not found in any Claude Code MCP configuration", serverName))
}

// claudeCodeScopes returns the Claude Code MCP configuration sources in diagnostic
// order: the managed config, the working directory's entries in the user config,
// the project files at and above the working directory, the user-wide servers table,
// and last the installed plugins.
//
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
	// The connector escape is handled where the closed outcome is answered, in
	// resolveClaudeCode.
	scopes := []scope{{
		path:   managedPath,
		key:    mcpServersKey,
		closed: true,
		load:   jsonServers(loader, managedPath),
	}}

	// More than one raw key can name the same directory after path normalization,
	// and the hook cannot safely choose one spelling on the agent's behalf.
	for _, key := range projectsKeys(env, claude, cwd) {
		scopes = append(scopes, scope{
			path: claudePath,
			key:  fmt.Sprintf("projects[%q].%s", key, mcpServersKey),
			load: fixedServers(decodeServers(claude.Projects[key].MCPServers), claudeRes),
		})
	}

	for _, path := range loader.projectFilePaths(cwd) {
		scopes = append(scopes, scope{
			path: path,
			key:  mcpServersKey,
			load: jsonServers(loader, path),
		})
	}

	scopes = append(scopes, scope{
		path: claudePath,
		key:  mcpServersKey,
		load: fixedServers(decodeServers(claude.MCPServers), claudeRes),
	})

	// Plugins are ordinary candidates too. A matching user, project, or local
	// declaration collides with a plugin declaration instead of shadowing it.
	plugin, gap := claudePluginScopes(ctx, loader, env, cwd, serverName)
	return append(scopes, plugin...), gap
}

// maxProjectDepth bounds the ancestor walk.
const maxProjectDepth = 40

// projectsKeys returns every key in the user config's per-project table that names
// the working directory, in stable order.
//
// That table is keyed by the directory the agent was launched in. An enclosing
// directory's entry belongs to a different session and never governs this call, so
// it is not consulted. Multiple raw keys that normalize to this directory are kept:
// the payload does not carry the original launch-path spelling needed to choose one.
//
// The directory here comes from the hook payload, which carries the agent's current
// working directory. That is the same directory in the ordinary case but not always:
// the agent anchors its configuration on the directory it started in, and a session
// whose working directory has since moved will disagree. The payload carries no way
// to recover the original, so the current directory is the best anchor available.
// TODO(g-linville): see if there is something we can do about this.
func projectsKeys(env Env, claude claudeJSON, cwd string) []string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return nil
	}
	want := env.comparableDir(cwd)

	var found []string
	for key := range claude.Projects {
		if env.comparableDir(key) == want {
			found = append(found, key)
		}
	}
	slices.Sort(found)
	return found
}

// projectFilePaths returns the project MCP files for cwd, caching them for the
// lifetime of one resolution.
func (l *configLoader) projectFilePaths(cwd string) []string {
	if cached, ok := l.projectFiles[cwd]; ok {
		return cached
	}
	paths := projectFilePaths(cwd)
	l.projectFiles[cwd] = paths
	return paths
}

// projectFilePaths returns the project MCP files that may govern a call made from
// cwd, nearest first.
//
// Every directory from the working directory upwards can hold one and the agent
// reads them all. They remain in nearest-first order for diagnostics, but every
// matching declaration is a candidate. The volume root is left out, which is where
// the agent's own walk stops.
//
// With no file anywhere, the working directory's own path is returned regardless, so
// that the file an operator expects to be read is named in the trace as absent
// rather than going unmentioned.
func projectFilePaths(cwd string) []string {
	paths := make([]string, 0, 4)
	nearest := ""
	for _, dir := range ancestors(cwd) {
		if isVolumeRoot(dir) {
			continue
		}
		path := filepath.Join(dir, claudeProjectMCPFile)
		if nearest == "" {
			nearest = path
		}
		if existsAsFile(path) {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 && nearest != "" {
		return []string{nearest}
	}
	return paths
}

// isVolumeRoot reports whether dir is the top of its volume.
func isVolumeRoot(dir string) bool {
	return filepath.Dir(dir) == dir
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
//
// On Windows the per-project table's keys are written with forward slashes, so
// separators are unified before cleaning; doing it in that order means both sides
// come out spelled the same whether this runs on Windows or on a host modeling it.
// A backslash is a legal filename character elsewhere, so this is confined to
// Windows.
//
// Case is folded there too, which the agent does not do — it writes and reads those
// keys through one function and so never sees a case difference, while the directory
// here arrives from a hook payload that may spell it differently. Folding is the
// tolerant side of that, and it is safe because only the one working directory is
// ever looked up.
func (e Env) comparableDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if e.windows() {
		return strings.ToLower(filepath.Clean(strings.ReplaceAll(path, `\`, "/")))
	}
	return filepath.Clean(path)
}

func resolveClaudeAIConnectors(claude claudeJSON, claudeRes loadResult, claudePath, serverName string, tr *tracer) []string {
	if !strings.HasPrefix(serverName, claudeAIConnectorPrefix) {
		return nil
	}
	if claudeRes != loadOK || len(claude.ClaudeAIMCPEverConnected) == 0 {
		tr.miss(claudePath, "claudeAiMcpEverConnected", claudeRes)
		return nil
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
	matches := matchNames(names, connected)
	if len(matches) > 0 {
		connectors := make([]string, 0, len(matches))
		for _, m := range matches {
			tr.hit(claudePath, "claudeAiMcpEverConnected", "connector "+m.key)
			connectors = append(connectors, m.key)
		}
		return connectors
	}
	tr.miss(claudePath, "claudeAiMcpEverConnected", claudeRes)
	return nil
}
