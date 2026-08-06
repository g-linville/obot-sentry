package enforce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Claude Code plugins declare MCP servers of their own, and a call to one arrives
// under a namespace no user configuration file mentions.
//
// The key fact is that a plugin server's internal name is "plugin:<plugin>:<server>"
// — colon-delimited, assembled with no sanitization — and the tool namespace is that
// name put through the ordinary Claude Code fold. Colon is not a legal namespace
// character, so it becomes an underscore, and that is the entire origin of the
// "plugin_<plugin>_<server>" shape a hook payload reports. Nothing here needs a
// namespace form of its own: synthesize the colon-delimited key and formClaudeCode
// matches it forward like any other configuration key.

const (
	// claudePluginKeyPrefix begins every plugin server's internal name.
	claudePluginKeyPrefix = "plugin:"
	// claudePluginNamespacePrefix is formClaudeCode of claudePluginKeyPrefix, and so
	// the prefix every plugin server's reported name carries.
	claudePluginNamespacePrefix = "plugin_"
	// claudePluginManifestSub is the plugin manifest, relative to the install root.
	claudePluginManifestSub = ".claude-plugin/plugin.json"
	// claudePluginMCPFile is the MCP server file every plugin may carry.
	claudePluginMCPFile = ".mcp.json"
)

// Bounds on plugin discovery.
const (
	maxPluginInstalls        = 256
	maxSkillDirEntries       = 512
	maxPluginManifestSources = 8
)

// pluginGap is a plugin source the resolver knows is there and could not enumerate:
// a directory it could not list, a bound it ran into, a registry it could not read.
type pluginGap struct {
	path   string
	key    string
	detail string
}

func firstGap(existing, next *pluginGap) *pluginGap {
	if existing != nil {
		return existing
	}
	return next
}

// unresolvedPluginGap is the denial a gap produces.
func unresolvedPluginGap(serverName string, gap *pluginGap, tr *tracer) Resolution {
	tr.gap(gap.path, gap.key, gap.detail)
	res := unresolved(serverName, fmt.Sprintf(
		"MCP server %q names a Claude Code plugin, and %s, so the installed plugins could not be enumerated and the hook cannot rule out another plugin declaring the same name",
		serverName, gap.detail))
	res.unenumerated = true
	return res
}

// claudePluginRegistry is the part of installed_plugins.json the resolver reads. It
// is keyed "<plugin>@<marketplace>", and a plugin can be installed more than once —
// once per scope, and once per project for the project scopes.
type claudePluginRegistry struct {
	Plugins map[string][]struct {
		Scope       string `json:"scope"`
		ProjectPath string `json:"projectPath"`
		InstallPath string `json:"installPath"`
	} `json:"plugins"`
}

// pluginInstall is one plugin installation on this machine.
type pluginInstall struct {
	// name is the plugin name, which is the middle segment of its server keys.
	name string
	// root is the absolute install directory. It is where the plugin's own files are
	// read from; it is deliberately not substituted into its entries, see namespaced.
	root string
}

// namespacePrefix is the prefix every reported name of this plugin's servers must
// carry.
func (in pluginInstall) namespacePrefix() string {
	return formClaudeCode(claudePluginKeyPrefix + in.name + ":")
}

// resolveRel resolves a manifest-declared path against the plugin root.
//
// A path leaving the root is refused, matching the guard Claude Code applies to
// repo-supplied plugins and correct to apply to all of them here. So is a .mcpb or
// .dxt bundle: unpacking an archive on the hot path is not something this resolver
// does, and a server we cannot see stays unresolved, which is the fail-closed side.
func (in pluginInstall) resolveRel(rel string) (string, bool) {
	rel = strings.TrimSpace(rel)
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "://") {
		return "", false
	}
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".mcpb", ".dxt":
		return "", false
	}
	joined := filepath.Join(in.root, filepath.FromSlash(rel))
	if joined != in.root && !strings.HasPrefix(joined, in.root+string(filepath.Separator)) {
		return "", false
	}
	return joined, true
}

// namespaced rewrites a plugin's own server table into the keys the agent uses for
// it.
func (in pluginInstall) namespaced(set serverSet) serverSet {
	out := make(serverSet, len(set))
	for name, entry := range set {
		out[claudePluginKeyPrefix+in.name+":"+name] = entry
	}
	return out
}

// claudePluginScopes returns the scopes contributed by this machine's Claude Code
// plugins, occupying rank for every plugin's manifest-declared sources and rank+1 for
// every plugin's own .mcp.json.
//
// Claude Code reads <root>/.mcp.json and then spreads the manifest's mcpServers over
// it, so the manifest wins within one plugin — hence the two ranks. Across plugins the
// scopes at a rank are peers, which is the point: two plugins whose keys fold to one
// namespace are a collision Claude Code resolves by silent last-wins, and peers make
// it an explicit ambiguity instead of a guess about which server ran.
func claudePluginScopes(ctx context.Context, loader *configLoader, env Env, cwd, serverName string, rank int) ([]scope, *pluginGap) {
	if !strings.HasPrefix(serverName, claudePluginNamespacePrefix) {
		return nil, nil
	}
	installs, gap := loader.claudePluginInstalls(ctx, env, cwd)
	var manifests, files []scope
	for _, in := range installs {
		if !strings.HasPrefix(serverName, in.namespacePrefix()) {
			continue
		}
		manifestScopes, manifestGap := pluginManifestScopes(ctx, loader, in, rank)
		gap = firstGap(gap, manifestGap)
		manifests = append(manifests, manifestScopes...)
		path := filepath.Join(in.root, claudePluginMCPFile)
		files = append(files, scope{
			path: path,
			key:  mcpServersKey,
			rank: rank + 1,
			load: pluginServers(loader, in, path),
		})
	}
	return append(manifests, files...), gap
}

// pluginManifestScopes returns the scopes for a plugin manifest's mcpServers, which
// may be an inline table, a path to a JSON file, or an array mixing the two.
//
// A manifest that declares nothing contributes no scope and no trace step: naming a
// file that was never going to say anything is noise. A declaration we cannot follow
// does contribute one, loading as unusable, because a source silently dropped is the
// one thing a resolution trace must never do.
func pluginManifestScopes(ctx context.Context, loader *configLoader, in pluginInstall, rank int) ([]scope, *pluginGap) {
	manifestPath := filepath.Join(in.root, filepath.FromSlash(claudePluginManifestSub))
	var doc struct {
		MCPServers json.RawMessage `json:"mcpServers"`
	}
	if res := loader.loadJSON(ctx, manifestPath, &doc); res != loadOK || len(doc.MCPServers) == 0 {
		return nil, nil
	}

	unusable := scope{
		path: manifestPath,
		key:  mcpServersKey,
		rank: rank,
		load: func(context.Context) (serverSet, loadResult) { return nil, loadUnusable },
	}

	out := make([]scope, 0, 2)
	for _, raw := range pluginManifestElements(doc.MCPServers) {
		if len(out) == maxPluginManifestSources {
			return out, &pluginGap{
				path: manifestPath,
				key:  mcpServersKey,
				detail: fmt.Sprintf("its manifest at %s declares more than %d MCP server sources",
					manifestPath, maxPluginManifestSources),
			}
		}
		switch jsonKindOf(raw) {
		case '{':
			var servers map[string]json.RawMessage
			if json.Unmarshal(raw, &servers) != nil {
				out = append(out, unusable)
				continue
			}
			out = append(out, scope{
				path: manifestPath,
				key:  mcpServersKey,
				rank: rank,
				load: fixedServers(in.namespaced(decodeServers(servers)), loadOK),
			})
		case '"':
			var rel string
			if json.Unmarshal(raw, &rel) != nil {
				out = append(out, unusable)
				continue
			}
			path, ok := in.resolveRel(rel)
			if !ok {
				out = append(out, unusable)
				continue
			}
			out = append(out, scope{
				path: path,
				key:  mcpServersKey,
				rank: rank,
				load: pluginServers(loader, in, path),
			})
		default:
			out = append(out, unusable)
		}
	}
	return out, nil
}

// pluginManifestElements flattens an array-valued mcpServers into its elements, and
// leaves any other value as the single element it is.
func pluginManifestElements(raw json.RawMessage) []json.RawMessage {
	if jsonKindOf(raw) != '[' {
		return []json.RawMessage{raw}
	}
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil {
		return []json.RawMessage{raw}
	}
	return elems
}

// jsonKindOf returns the first significant byte of a JSON value, which is enough to
// tell an object from a string from an array. Zero for an empty value.
func jsonKindOf(raw json.RawMessage) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b
		}
	}
	return 0
}

// pluginServers loads a plugin's MCP server file into the agent's namespaced keys.
//
// A plugin file is parsed by a laxer reader than user and project configuration:
// Claude Code takes `doc.mcpServers || doc`, so the servers may sit under the usual
// wrapper or be the whole document. Both are accepted here for the same reason the
// resolver tolerates JSONC — a file we refuse to read becomes an unresolved call, and
// an unresolved call is denied. It also means a bare-map server named "mcpServers" is
// misread as the wrapper, which is exactly what the agent does with it.
func pluginServers(loader *configLoader, in pluginInstall, path string) func(context.Context) (serverSet, loadResult) {
	return func(ctx context.Context) (serverSet, loadResult) {
		if ctx.Err() != nil {
			return nil, loadUnusable
		}
		var doc map[string]json.RawMessage
		res := loader.loadJSON(ctx, path, &doc)
		if res != loadOK {
			return nil, res
		}
		raw := doc
		if wrapped, ok := doc[mcpServersKey]; ok && jsonKindOf(wrapped) == '{' {
			raw = nil
			if json.Unmarshal(wrapped, &raw) != nil {
				return nil, loadUnusable
			}
		}
		if ctx.Err() != nil {
			return nil, loadUnusable
		}
		return in.namespaced(decodeServers(raw)), loadOK
	}
}

// claudePluginInstalls returns the plugin installations that may govern a call made
// from cwd, caching them for the lifetime of one resolution. The cache is what keeps
// an ambiguous tool name, which resolves once per way it splits, from re-reading the
// registry and re-walking the skill directories each time.
func (l *configLoader) claudePluginInstalls(ctx context.Context, env Env, cwd string) ([]pluginInstall, *pluginGap) {
	if ctx.Err() != nil {
		return nil, nil
	}
	if cached, ok := l.pluginInstalls[cwd]; ok {
		return cached.installs, cached.gap
	}
	installs, gap := registryPluginInstalls(ctx, l, env, cwd, make([]pluginInstall, 0, 8))
	installs, skillsGap := skillsDirPluginInstalls(env, cwd, installs)
	gap = firstGap(gap, skillsGap)
	if ctx.Err() != nil {
		return nil, nil
	}
	l.pluginInstalls[cwd] = cachedPluginInstalls{installs: installs, gap: gap}
	return installs, gap
}

// registryPluginInstalls appends the installations recorded in the plugin registry.
//
// A project- or local-scoped installation records the project it belongs to, and one
// belonging to a different project cannot have served this call. Dropping it is not a
// judgement about whether the plugin would have run: it is the same question
// projectScopes already answers, and keeping it would only manufacture ambiguity
// between two install paths for one name.
func registryPluginInstalls(ctx context.Context, loader *configLoader, env Env, cwd string, out []pluginInstall) ([]pluginInstall, *pluginGap) {
	var reg claudePluginRegistry
	// The v2 registry supersedes the original where it exists; both hold the same
	// shape, a plugin id mapping to one installation per scope.
	path := env.homePath(".claude", "plugins", "installed_plugins_v2.json")
	res := loader.loadJSON(ctx, path, &reg)
	if res != loadOK || len(reg.Plugins) == 0 {
		v2Res := res
		reg = claudePluginRegistry{}
		path = env.homePath(".claude", "plugins", "installed_plugins.json")
		if res = loader.loadJSON(ctx, path, &reg); res != loadOK {
			// A registry that is merely absent means there is nothing to enumerate. One
			// that is there and unreadable is a gap and not the ordinary
			// malformed-config case, because this file is not a source of servers: it
			// is the index naming every file that is. Losing it hides the whole plugin
			// set rather than one entry in it.
			if res == loadUnusable {
				return out, &pluginGap{path: path, key: "plugins", detail: fmt.Sprintf(
					"the plugin registry at %s could not be read", path)}
			}
			if v2Res == loadUnusable {
				v2Path := env.homePath(".claude", "plugins", "installed_plugins_v2.json")
				return out, &pluginGap{path: v2Path, key: "plugins", detail: fmt.Sprintf(
					"the plugin registry at %s could not be read", v2Path)}
			}
			return out, nil
		}
	}

	dirs := ancestors(cwd)
	projectDirs := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		projectDirs[env.comparableDir(dir)] = struct{}{}
	}

	seen := make(map[string]struct{}, len(reg.Plugins))
	// Sorted, so a registry that overruns the bound drops the same installations on
	// every run rather than whichever ones map order happened to reach last.
	for _, key := range slices.Sorted(maps.Keys(reg.Plugins)) {
		name, _, _ := strings.Cut(key, "@")
		if name == "" {
			continue
		}
		for _, install := range reg.Plugins[key] {
			root := strings.TrimSpace(install.InstallPath)
			if root == "" || !filepath.IsAbs(root) {
				continue
			}
			root = filepath.Clean(root)
			if project := strings.TrimSpace(install.ProjectPath); project != "" {
				if _, ok := projectDirs[env.comparableDir(project)]; !ok {
					continue
				}
			}
			if _, ok := seen[name+"\x00"+root]; ok {
				continue
			}
			if len(out) == maxPluginInstalls {
				return out, &pluginGap{path: path, key: "plugins", detail: fmt.Sprintf(
					"more than %d plugin installations are recorded in %s",
					maxPluginInstalls, path)}
			}
			seen[name+"\x00"+root] = struct{}{}
			out = append(out, pluginInstall{name: name, root: root})
		}
	}
	return out, nil
}

// skillsDirPluginInstalls appends the pseudo-plugins Claude Code synthesizes from
// skill directories. They appear in no registry — the agent builds one per directory
// under the global and project skills roots, named for the directory — and they carry
// no manifest, so a .mcp.json in the directory is all there is to read.
func skillsDirPluginInstalls(env Env, cwd string, out []pluginInstall) ([]pluginInstall, *pluginGap) {
	roots := []string{env.homePath(".claude", "skills")}
	if dir := nearestSkillsDir(cwd); dir != "" {
		roots = append(roots, dir)
	}
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}

		entries, err := os.ReadDir(root)
		switch {
		case isNotDirectory(err):
			// Nothing there to enumerate, which is the ordinary case on most machines.
			continue
		case err != nil:
			return out, &pluginGap{path: root, detail: fmt.Sprintf(
				"the skill directory %s could not be read", root)}
		case len(entries) > maxSkillDirEntries:
			return out, &pluginGap{path: root, detail: fmt.Sprintf(
				"the skill directory %s holds more than %d entries",
				root, maxSkillDirEntries)}
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if len(out) == maxPluginInstalls {
				return out, &pluginGap{path: root, detail: fmt.Sprintf(
					"more than %d plugin installations were found", maxPluginInstalls)}
			}
			out = append(out, pluginInstall{
				name: entry.Name(),
				root: filepath.Join(root, entry.Name()),
			})
		}
	}
	return out, nil
}

func isNotDirectory(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// nearestSkillsDir returns the .claude/skills directory of the first ancestor of cwd
// that has one, mirroring nearestProjectFileDir. Unlike that one it returns nothing
// when there is none: an absent skills directory contributes no plugin, so there is
// no source for the trace to name.
func nearestSkillsDir(cwd string) string {
	for _, dir := range ancestors(cwd) {
		candidate := filepath.Join(dir, ".claude", "skills")
		info, err := os.Lstat(candidate)
		switch {
		case err == nil && info.IsDir():
			return candidate
		case err != nil && !isNotDirectory(err):
			return candidate
		}
	}
	return ""
}
