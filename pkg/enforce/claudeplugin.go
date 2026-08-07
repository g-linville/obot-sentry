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
	// claudeInstalledPluginsFile is the plugin registry, relative to the plugin tree.
	// It is the only registry file there is: its contents carry their own version, so
	// this one name covers every layout the agent reads.
	claudeInstalledPluginsFile = "installed_plugins.json"
	// claudeKnownMarketplacesFile indexes the marketplaces plugins come from, relative
	// to the plugin tree.
	claudeKnownMarketplacesFile = "known_marketplaces.json"
	// claudeMarketplaceManifestSub is a marketplace's own manifest, relative to where
	// it is installed.
	claudeMarketplaceManifestSub = ".claude-plugin/marketplace.json"
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

// claudePluginRegistry is the part of the plugin registry the resolver reads. It is
// keyed "<plugin>@<marketplace>".
//
// The value is kept raw because it has two shapes across registry versions and a
// machine can be carrying either: the current one lists every installation of a
// plugin, and the older one records a single installation as a bare object. Decoding
// a whole file under one of those shapes would reject the other outright, which for
// this file means losing the entire plugin set. See pluginInstallRecords.
type claudePluginRegistry struct {
	Plugins map[string]json.RawMessage `json:"plugins"`
}

// pluginInstallRecord is one installation of one plugin. A plugin can be installed
// more than once — once per scope, and once per project for the project scopes.
type pluginInstallRecord struct {
	ProjectPath string `json:"projectPath"`
	InstallPath string `json:"installPath"`
	Version     string `json:"version"`
}

// pluginInstallRecords decodes one registry entry under whichever shape it has.
//
// The older shape does not record where the plugin was installed to; that location
// is implied by the plugin's identity and version instead. It can sit in the main
// plugin tree or in any of the seeded ones, so every candidate is returned, and the
// ones that are not there contribute nothing.
func pluginInstallRecords(env Env, id string, raw json.RawMessage) []pluginInstallRecord {
	switch jsonKindOf(raw) {
	case '[':
		var records []pluginInstallRecord
		if json.Unmarshal(raw, &records) != nil {
			return nil
		}
		return records
	case '{':
		var record pluginInstallRecord
		if json.Unmarshal(raw, &record) != nil {
			return nil
		}
		if record.InstallPath != "" {
			return []pluginInstallRecord{record}
		}
		paths := pluginCachePaths(env, id, record.Version)
		records := make([]pluginInstallRecord, 0, len(paths))
		for _, path := range paths {
			records = append(records, pluginInstallRecord{InstallPath: path})
		}
		return records
	}
	return nil
}

// claudeMarketplaces is the part of the marketplace index the resolver reads: for
// each marketplace, where it is served from and where it lives on this machine.
type claudeMarketplaces map[string]struct {
	Source struct {
		Source string `json:"source"`
	} `json:"source"`
	InstallLocation string `json:"installLocation"`
}

// servedInPlace reports whether a marketplace is a directory on this machine rather
// than something fetched from elsewhere. That distinction decides where the plugins
// under it are read from — see marketplacePluginRoot.
func (m claudeMarketplaces) servedInPlace(name string) (string, bool) {
	entry, ok := m[name]
	if !ok {
		return "", false
	}
	switch entry.Source.Source {
	case "file", "directory":
		return strings.TrimSpace(entry.InstallLocation), true
	}
	return "", false
}

// loadMarketplaces reads the index of the marketplaces installed on this machine.
//
// An index that is absent records no marketplace at all. Nothing is then served in
// place, so every plugin is read from the location its own registry entry names, and
// there is nothing here to resolve. That is the ordinary case, and not a gap.
//
// An index that exists and cannot be read is a gap, for the reason an unreadable
// plugin registry is one: it declares no servers itself, it decides which files do.
// Without it there is no telling whether a plugin is read from its marketplace or
// from the copy the registry recorded — two different files, which can declare two
// different servers.
func loadMarketplaces(ctx context.Context, loader *configLoader, env Env) (claudeMarketplaces, *pluginGap) {
	var markets claudeMarketplaces
	path := filepath.Join(env.claudePluginsDir(), claudeKnownMarketplacesFile)
	switch loader.loadJSON(ctx, path, &markets) {
	case loadUnusable:
		return nil, &pluginGap{path: path, key: "marketplaces", detail: fmt.Sprintf(
			"the marketplace index at %s could not be read", path)}
	case loadAbsent:
		return nil, nil
	}
	return markets, nil
}

// marketplacePluginRoot returns the directory a plugin's own files are read from when
// its marketplace is served in place, and empty when the registry's recorded location
// governs instead.
//
// The marketplace names each plugin's directory relative to its own, so that entry is
// what locates the plugin — the plugin's name is only the key to look it up by. An
// index that names a location which is itself a file is read relative to the
// directory holding it.
func marketplacePluginRoot(ctx context.Context, loader *configLoader, markets claudeMarketplaces, name, marketplace string) (string, *pluginGap) {
	base, ok := markets.servedInPlace(marketplace)
	if !ok || base == "" || !filepath.IsAbs(base) {
		return "", nil
	}
	if info, err := os.Lstat(base); err == nil && !info.IsDir() {
		base = filepath.Dir(base)
	}

	var manifest struct {
		Plugins []struct {
			Name   string          `json:"name"`
			Source json.RawMessage `json:"source"`
		} `json:"plugins"`
	}
	path := filepath.Join(base, filepath.FromSlash(claudeMarketplaceManifestSub))
	if res := loader.loadJSON(ctx, path, &manifest); res != loadOK {
		if res == loadUnusable {
			return "", &pluginGap{path: path, key: "plugins", detail: fmt.Sprintf(
				"the marketplace at %s could not be read", path)}
		}
		return "", nil
	}

	for _, entry := range manifest.Plugins {
		if entry.Name != name {
			continue
		}
		// Only a plugin named as a path under the marketplace is read in place. One
		// carrying a source of its own is fetched like any other, so the registry's
		// recorded location governs it.
		var rel string
		if jsonKindOf(entry.Source) != '"' || json.Unmarshal(entry.Source, &rel) != nil {
			return "", nil
		}
		if rel = strings.TrimSpace(rel); rel == "" || filepath.IsAbs(rel) {
			return "", nil
		}
		return filepath.Join(base, filepath.FromSlash(rel)), nil
	}
	return "", nil
}

// pluginCachePaths returns the locations the older registry shape implies a plugin
// was installed to: under a plugin tree's cache, keyed by marketplace, plugin name,
// and version, each reduced to a single path segment.
func pluginCachePaths(env Env, id, version string) []string {
	name, marketplace, _ := strings.Cut(id, "@")
	if name == "" {
		name = id
	}
	if marketplace == "" {
		marketplace = "unknown"
	}
	if version == "" {
		version = "unknown"
	}
	rel := filepath.Join("cache",
		pluginPathSegment(marketplace, false),
		pluginPathSegment(name, false),
		pluginPathSegment(version, true))

	roots := append([]string{env.claudePluginsDir()}, env.claudePluginSeedDirs()...)
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		out = append(out, filepath.Join(root, rel))
	}
	return out
}

// pluginPathSegment reduces a value to the single path segment the plugin tree names
// it with: everything outside letters, digits, hyphen, and underscore becomes a
// hyphen. A version segment keeps dots too, except that one made of nothing but dots
// would name a directory relative to its parent rather than a new one.
func pluginPathSegment(s string, version bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case version && r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if out := b.String(); out != "." && out != ".." {
		return out
	}
	return "-"
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
	installs, skillsGap := skillsDirPluginInstalls(ctx, l, env, cwd, installs)
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
	// The registry lives in exactly one file, whose contents carry their own version.
	path := filepath.Join(env.claudePluginsDir(), claudeInstalledPluginsFile)
	if res := loader.loadJSON(ctx, path, &reg); res != loadOK {
		// A registry that is merely absent means there is nothing to enumerate. One
		// that is there and unreadable is a gap and not the ordinary malformed-config
		// case, because this file is not a source of servers: it is the index naming
		// every file that is. Losing it hides the whole plugin set rather than one
		// entry in it.
		if res == loadUnusable {
			return out, &pluginGap{path: path, key: "plugins", detail: fmt.Sprintf(
				"the plugin registry at %s could not be read", path)}
		}
		return out, nil
	}

	markets, gap := loadMarketplaces(ctx, loader, env)
	if gap != nil {
		return out, gap
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
		name, marketplace, _ := strings.Cut(key, "@")
		if name == "" {
			continue
		}
		// A marketplace already on this machine is used where it sits, and the
		// location the registry records for the plugin is not consulted at all. That
		// location may be an absent or stale copy of a directory somebody is editing
		// in place, so preferring it would read a file the agent never opens.
		inPlace, marketGap := marketplacePluginRoot(ctx, loader, markets, name, marketplace)
		if marketGap != nil {
			return out, marketGap
		}

		for _, install := range pluginInstallRecords(env, key, reg.Plugins[key]) {
			root := strings.TrimSpace(install.InstallPath)
			if inPlace != "" {
				root = inPlace
			}
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
// skill directories. They appear in no registry — the agent builds one per
// qualifying directory under the global and project skills roots, named for the
// directory it found rather than for anything the directory calls itself.
//
// A directory qualifies on one thing: carrying a plugin manifest. Without one it is
// a skill and nothing more, whatever else it holds, so treating it as a plugin would
// let a server file that never loads answer for a call.
//
// These carry that manifest like any other plugin, so what it declares is read the
// same way — see pluginManifestScopes.
func skillsDirPluginInstalls(ctx context.Context, loader *configLoader, env Env, cwd string, out []pluginInstall) ([]pluginInstall, *pluginGap) {
	roots := []string{env.claudeSkillsDir()}
	// The project root is the working directory's own, with no walk above it: the
	// agent looks where it was launched and nowhere else. See projectsKey on why that
	// directory is the best anchor a hook payload can offer.
	if dir := projectSkillsDir(cwd); dir != "" {
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
			// A symlinked directory counts too, which is how one skill tree is
			// commonly shared between projects.
			if !entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			if !dirDeclaresPlugin(ctx, loader, dir) {
				continue
			}
			if len(out) == maxPluginInstalls {
				return out, &pluginGap{path: root, detail: fmt.Sprintf(
					"more than %d plugin installations were found", maxPluginInstalls)}
			}
			out = append(out, pluginInstall{name: entry.Name(), root: dir})
		}
	}
	return out, nil
}

// dirDeclaresPlugin reports whether a directory carries a plugin manifest, which is
// what makes the servers under it live.
//
// The manifest has to parse as an object, not merely exist: a malformed one leaves
// the agent with no plugin at all rather than a plugin with nothing in it.
func dirDeclaresPlugin(ctx context.Context, loader *configLoader, dir string) bool {
	var manifest struct {
		Name string `json:"name"`
	}
	path := filepath.Join(dir, filepath.FromSlash(claudePluginManifestSub))
	return loader.loadJSON(ctx, path, &manifest) == loadOK
}

func isNotDirectory(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// projectSkillsDir is the project skills root for a call made from cwd, or nothing
// when cwd cannot identify one. An absent skills directory contributes no plugin, so
// there is no source for the trace to name.
func projectSkillsDir(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	return filepath.Join(filepath.Clean(cwd), ".claude", "skills")
}
