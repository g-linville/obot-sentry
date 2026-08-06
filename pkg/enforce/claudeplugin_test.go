package enforce

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obot-platform/obot-sentry/pkg/localagent"
)

// installPlugin writes a registry entry for one user-scoped plugin and returns its
// install root.
func (f *fixture) installPlugin(key string) string {
	f.t.Helper()
	name, _, _ := strings.Cut(key, "@")
	root := f.homePath(".claude", "plugins", "cache", "market", name, "1.0.0")
	f.mkdir(root)
	f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), fmt.Sprintf(
		`{"version":2,"plugins":{%s:[{"scope":"user","installPath":%s,"version":"1.0.0"}]}}`,
		quote(key), quote(root)))
	return root
}

// TestClaudeCodePluginResolvesFromMCPJSON is the shape every plugin on a real
// machine has: a registry entry and a .mcp.json holding a bare map of servers.
func TestClaudeCodePluginResolvesFromMCPJSON(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("context7@claude-plugins-official")
	f.write(filepath.Join(root, ".mcp.json"),
		`{"context7":{"command":"npx","args":["-y","@upstash/context7-mcp"]}}`)

	res := Resolve(f.Env, claudeCodeReq("plugin_context7_context7", f.path("proj")))
	assertPackage(t, res, "npm", "@upstash/context7-mcp", "")

	// The colon-delimited key, not the folded name the call reported: that key is
	// what the agent itself displays, and the folded form appears in no file.
	if res.ServerName != "plugin:context7:context7" {
		t.Fatalf("server name = %q, want %q\n%s", res.ServerName, "plugin:context7:context7", resolveTrace(res))
	}
	last := res.Trace[len(res.Trace)-1]
	if want := filepath.Join(root, ".mcp.json"); last.Path != want || !last.Matched {
		t.Fatalf("expected the match on %s:\n%s", want, resolveTrace(res))
	}
}

// TestClaudeCodePluginMCPJSONWrapperForm covers the other shape a plugin file comes
// in. Claude Code reads `doc.mcpServers || doc`, so both have to work.
func TestClaudeCodePluginMCPJSONWrapperForm(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("discord@claude-plugins-official")
	f.write(filepath.Join(root, ".mcp.json"),
		`{"mcpServers":{"discord":{"url":"https://discord.example.com/mcp"}}}`)

	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_discord_discord", f.path("proj"))),
		"https://discord.example.com/mcp")
}

// TestClaudeCodePluginManifestBeatsMCPJSON covers the merge order inside one plugin:
// the agent spreads the manifest's table over the file's, so the manifest wins.
func TestClaudeCodePluginManifestBeatsMCPJSON(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("acme@market")
	f.write(filepath.Join(root, ".mcp.json"),
		`{"tools":{"url":"https://file.example.com/mcp"}}`)
	manifest := f.write(filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"acme","mcpServers":{"tools":{"url":"https://manifest.example.com/mcp"}}}`)

	res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj")))
	assertURL(t, res, "https://manifest.example.com/mcp")
	if last := res.Trace[len(res.Trace)-1]; last.Path != manifest {
		t.Fatalf("expected the match on the manifest:\n%s", resolveTrace(res))
	}
}

// TestClaudeCodePluginManifestStringPath covers a manifest that points at a file
// instead of declaring servers inline.
func TestClaudeCodePluginManifestStringPath(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("acme@market")
	f.write(filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"acme","mcpServers":"config/servers.json"}`)
	servers := f.write(filepath.Join(root, "config", "servers.json"),
		`{"mcpServers":{"tools":{"url":"https://pointed.example.com/mcp"}}}`)

	res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj")))
	assertURL(t, res, "https://pointed.example.com/mcp")
	if last := res.Trace[len(res.Trace)-1]; last.Path != servers {
		t.Fatalf("expected the match on %s:\n%s", servers, resolveTrace(res))
	}
}

// TestClaudeCodePluginManifestUnusableSources covers the declarations we will not
// follow. Each still contributes a step, because a source dropped without a word is
// the one thing a trace must not do.
func TestClaudeCodePluginManifestUnusableSources(t *testing.T) {
	for name, declaration := range map[string]string{
		"escapes the root": `"../../elsewhere/servers.json"`,
		"absolute path":    `"/etc/servers.json"`,
		"bundle":           `"bundled.mcpb"`,
		"remote bundle":    `"https://example.com/servers.dxt"`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "darwin")
			root := f.installPlugin("acme@market")
			manifest := f.write(filepath.Join(root, ".claude-plugin", "plugin.json"),
				`{"name":"acme","mcpServers":`+declaration+`}`)
			f.write(filepath.Join(root, ".mcp.json"),
				`{"tools":{"url":"https://file.example.com/mcp"}}`)

			// The refused source does not stop the plugin's own file from answering.
			res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj")))
			assertURL(t, res, "https://file.example.com/mcp")

			step := res.Trace[len(res.Trace)-2]
			if step.Path != manifest || step.Note != "unreadable or malformed" {
				t.Fatalf("expected the manifest recorded as unusable:\n%s", resolveTrace(res))
			}
		})
	}
}

// TestClaudeCodePluginManifestArray covers the array form, which mixes inline tables
// and paths in one declaration.
func TestClaudeCodePluginManifestArray(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("acme@market")
	f.write(filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"acme","mcpServers":["config/a.json",{"inline":{"url":"https://inline.example.com/mcp"}}]}`)
	f.write(filepath.Join(root, "config", "a.json"),
		`{"pointed":{"url":"https://pointed.example.com/mcp"}}`)

	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_pointed", f.path("proj"))),
		"https://pointed.example.com/mcp")
	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_inline", f.path("proj"))),
		"https://inline.example.com/mcp")
}

// TestClaudeCodePluginManifestSourcesAreBounded covers the cap on how many sources
// one manifest can name. Running into it denies every plugin name, including the ones
// the sources we did read would have answered.
func TestClaudeCodePluginManifestSourcesAreBounded(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("acme@market")

	elems := make([]string, 0, maxPluginManifestSources+1)
	for i := range maxPluginManifestSources + 1 {
		elems = append(elems, fmt.Sprintf(`{"s%d":{"url":"https://s%d.example.com/mcp"}}`, i, i))
	}
	f.write(filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"acme","mcpServers":[`+strings.Join(elems, ",")+`]}`)

	assertUnresolved(t, Resolve(f.Env, claudeCodeReq("plugin_acme_s0", f.path("proj"))),
		fmt.Sprintf("declares more than %d MCP server sources", maxPluginManifestSources))
	assertUnresolved(t, Resolve(f.Env, claudeCodeReq(
		fmt.Sprintf("plugin_acme_s%d", maxPluginManifestSources), f.path("proj"))),
		fmt.Sprintf("declares more than %d MCP server sources", maxPluginManifestSources))

	// One under the bound reads normally.
	f.write(filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"acme","mcpServers":[`+strings.Join(elems[:maxPluginManifestSources], ",")+`]}`)
	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_s0", f.path("proj"))), "https://s0.example.com/mcp")
}

// TestClaudeCodePluginRootSubstitution covers the plugin-scoped variables, which are
// the difference between a real command and a literal ${...}.
func TestClaudeCodePluginRootSubstitution(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("acme@market")
	f.write(filepath.Join(root, ".mcp.json"),
		`{"tools":{"command":"${CLAUDE_PLUGIN_ROOT}/bin/server","args":["--data","${CLAUDE_PLUGIN_DATA}","--project","${CLAUDE_PROJECT_DIR}"]}}`)

	project := f.path("proj")
	res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", project))

	// A path is not a package runner, so this is unresolved-with-command — which is
	// exactly the state that proves the substitution happened before identification.
	assertUnresolved(t, res, "is a path, not a bare package runner")
	if want := filepath.Join(root, "bin", "server"); res.Identity.Command != want {
		t.Fatalf("command = %q, want %q\n%s", res.Identity.Command, want, resolveTrace(res))
	}
}

// TestClaudeCodePluginNamespaceFolding covers a plugin and a server whose names do
// not survive namespacing, which is the case an exact-match lookup would miss.
func TestClaudeCodePluginNamespaceFolding(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("my.plugin@market")
	f.write(filepath.Join(root, ".mcp.json"), `{"my server":{"url":"https://folded.example.com/mcp"}}`)

	res := Resolve(f.Env, claudeCodeReq("plugin_my_plugin_my_server", f.path("proj")))
	assertURL(t, res, "https://folded.example.com/mcp")
	if res.ServerName != "plugin:my.plugin:my server" {
		t.Fatalf("server name = %q\n%s", res.ServerName, resolveTrace(res))
	}
}

// TestClaudeCodePluginNamespacePrefixMatchesTheForm pins the constant that gates the
// whole path against the transform it stands for.
func TestClaudeCodePluginNamespacePrefixMatchesTheForm(t *testing.T) {
	if got := formClaudeCode(claudePluginKeyPrefix); got != claudePluginNamespacePrefix {
		t.Fatalf("formClaudeCode(%q) = %q, want %q", claudePluginKeyPrefix, got, claudePluginNamespacePrefix)
	}
	// The prefix filter relies on folding a joined key being the same as folding its
	// parts, which holds because the transform never looks behind the character it is
	// replacing.
	for _, name := range []string{"acme", "my.plugin", "a-b_c", "acme/tools", "ünïcode"} {
		joined := formClaudeCode(claudePluginKeyPrefix + name + ":server")
		parts := formClaudeCode(claudePluginKeyPrefix+name+":") + formClaudeCode("server")
		if joined != parts {
			t.Fatalf("fold of %q is not prefix-composable: %q vs %q", name, joined, parts)
		}
	}
}

// TestClaudeCodePluginCollisionIsAmbiguous covers two plugins whose keys fold to one
// namespace. Claude Code settles that by silent last-wins; the hook cannot say which
// server ran, so it says so.
func TestClaudeCodePluginCollisionIsAmbiguous(t *testing.T) {
	f := newFixture(t, "darwin")
	fooRoot := f.homePath(".claude", "plugins", "cache", "market", "foo", "1.0.0")
	fooBarRoot := f.homePath(".claude", "plugins", "cache", "market", "foo_bar", "1.0.0")
	f.mkdir(fooRoot)
	f.mkdir(fooBarRoot)
	f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), fmt.Sprintf(
		`{"plugins":{"foo@market":[{"scope":"user","installPath":%s}],"foo_bar@market":[{"scope":"user","installPath":%s}]}}`,
		quote(fooRoot), quote(fooBarRoot)))

	// Both fold to plugin_foo_bar_baz.
	f.write(filepath.Join(fooRoot, ".mcp.json"), `{"bar_baz":{"url":"https://foo.example.com/mcp"}}`)
	f.write(filepath.Join(fooBarRoot, ".mcp.json"), `{"baz":{"url":"https://foobar.example.com/mcp"}}`)

	assertUnresolved(t, Resolve(f.Env, claudeCodeReq("plugin_foo_bar_baz", f.path("proj"))),
		"conflicting definitions")

	// Peers that repeat one definition identify the server just as well as one scope
	// would, so they resolve rather than denying.
	f.write(filepath.Join(fooBarRoot, ".mcp.json"), `{"baz":{"url":"https://foo.example.com/mcp"}}`)
	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_foo_bar_baz", f.path("proj"))),
		"https://foo.example.com/mcp")
}

// TestClaudeCodePluginUserConfigWins covers the precedence rule. Nothing reserves the
// plugin namespace, and the agent lets user configuration overwrite a plugin key.
func TestClaudeCodePluginUserConfigWins(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("acme@market")
	f.write(filepath.Join(root, ".mcp.json"), `{"tools":{"url":"https://plugin.example.com/mcp"}}`)
	f.write(f.homePath(".claude.json"),
		`{"mcpServers":{"plugin:acme:tools":{"url":"https://user.example.com/mcp"}}}`)

	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj"))),
		"https://user.example.com/mcp")
}

// TestClaudeCodePluginManagedConfigIsExclusive covers the managed lockdown reaching
// plugin servers. The agent returns the managed set before plugin discovery runs, so
// a managed file that lacks the name ends resolution here too.
func TestClaudeCodePluginManagedConfigIsExclusive(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("acme@market")
	f.write(filepath.Join(root, ".mcp.json"), `{"tools":{"url":"https://plugin.example.com/mcp"}}`)
	f.write(f.machinePath(claudeManagedMCPDarwin), `{"mcpServers":{"github":{"url":"https://managed.example.com/sse"}}}`)

	res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj")))
	assertUnresolved(t, res, "managed MCP configuration, which cannot be overridden")
	if len(res.Trace) != 1 {
		t.Fatalf("resolution continued past the managed config:\n%s", resolveTrace(res))
	}
}

// TestClaudeCodePluginSkillsDir covers the pseudo-plugins the agent synthesizes from
// skill directories. They are in no registry, so a skills tree is the only evidence
// they exist.
func TestClaudeCodePluginSkillsDir(t *testing.T) {
	f := newFixture(t, "darwin")
	project := f.path("proj")

	global := f.mkdir(f.homePath(".claude", "skills", "notes"))
	f.write(filepath.Join(global, ".mcp.json"), `{"notes":{"url":"https://global-skill.example.com/mcp"}}`)
	local := f.mkdir(filepath.Join(project, ".claude", "skills", "deploy"))
	f.write(filepath.Join(local, ".mcp.json"), `{"deploy":{"url":"https://project-skill.example.com/mcp"}}`)

	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_notes_notes", project)), "https://global-skill.example.com/mcp")
	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_deploy_deploy", project)), "https://project-skill.example.com/mcp")

	// The project skills root is found from a subdirectory too, the way the agent
	// finds the project it was launched in.
	// TODO(g-linville): verify that this is how Claude Code actually works.
	deep := f.mkdir(filepath.Join(project, "src", "pkg"))
	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_deploy_deploy", deep)), "https://project-skill.example.com/mcp")

	// A skills plugin carries no manifest, so only its own file is consulted.
	res := Resolve(f.Env, claudeCodeReq("plugin_deploy_deploy", project))
	if last := res.Trace[len(res.Trace)-1]; last.Path != filepath.Join(local, ".mcp.json") {
		t.Fatalf("expected the match on the skill's own file:\n%s", resolveTrace(res))
	}
}

// TestClaudeCodePluginSkillsDirDataPath covers the data directory a skills
// pseudo-plugin gets, which is named for its synthetic marketplace.
func TestClaudeCodePluginSkillsDirDataPath(t *testing.T) {
	f := newFixture(t, "darwin")
	skill := f.mkdir(f.homePath(".claude", "skills", "notes"))
	f.write(filepath.Join(skill, ".mcp.json"), `{"notes":{"command":"${CLAUDE_PLUGIN_DATA}/run"}}`)

	res := Resolve(f.Env, claudeCodeReq("plugin_notes_notes", f.path("proj")))
	want := f.homePath(".claude", "plugins", "data", "notes-skills-dir", "run")
	if res.Identity.Command != want {
		t.Fatalf("command = %q, want %q\n%s", res.Identity.Command, want, resolveTrace(res))
	}
}

// TestClaudeCodePluginProjectScopeInstallIgnoredElsewhere covers an installation
// belonging to another project. It cannot have served this call, and keeping it would
// only manufacture ambiguity between two roots for one name.
func TestClaudeCodePluginProjectScopeInstallIgnoredElsewhere(t *testing.T) {
	f := newFixture(t, "darwin")
	mine := f.path("mine")
	theirs := f.path("theirs")
	root := f.mkdir(f.homePath("elsewhere"))
	f.write(filepath.Join(root, ".mcp.json"), `{"tools":{"url":"https://theirs.example.com/mcp"}}`)
	f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), fmt.Sprintf(
		`{"plugins":{"acme@market":[{"scope":"project","projectPath":%s,"installPath":%s}]}}`,
		quote(theirs), quote(root)))

	res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", mine))
	assertUnresolved(t, res, "was not found")
	for _, step := range res.Trace {
		if strings.HasPrefix(step.Path, root) {
			t.Fatalf("consulted another project's installation:\n%s", resolveTrace(res))
		}
	}

	// From inside the project that owns it, it answers.
	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", theirs)), "https://theirs.example.com/mcp")
}

// TestClaudeCodePluginRegistryV2Supersedes covers the newer registry filename taking
// precedence over the original.
func TestClaudeCodePluginRegistryV2Supersedes(t *testing.T) {
	f := newFixture(t, "darwin")
	old := f.mkdir(f.homePath("old"))
	current := f.mkdir(f.homePath("current"))
	f.write(filepath.Join(old, ".mcp.json"), `{"tools":{"url":"https://old.example.com/mcp"}}`)
	f.write(filepath.Join(current, ".mcp.json"), `{"tools":{"url":"https://current.example.com/mcp"}}`)
	// TODO(g-linville): make sure this is how Claude Code actually works.
	f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), fmt.Sprintf(
		`{"plugins":{"acme@market":[{"scope":"user","installPath":%s}]}}`, quote(old)))
	f.write(f.homePath(".claude", "plugins", "installed_plugins_v2.json"), fmt.Sprintf(
		`{"plugins":{"acme@market":[{"scope":"user","installPath":%s}]}}`, quote(current)))

	assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj"))),
		"https://current.example.com/mcp")
}

// TestClaudeCodePluginMalformedFilesAreSkipped covers the tolerance rule on this
// path: an unparseable registry or plugin file is skipped, not fatal.
func TestClaudeCodePluginMalformedFilesAreSkipped(t *testing.T) {
	// The registry is the exception: it is not a source of servers but the index
	// naming every file that is, so losing it hides the whole plugin set and denies
	// rather than falling through. See TestClaudeCodePluginUnenumerableSourcesDeny.
	t.Run("plugin file", func(t *testing.T) {
		f := newFixture(t, "darwin")
		root := f.installPlugin("acme@market")
		f.write(filepath.Join(root, ".mcp.json"), `{"tools":`)
		f.write(f.homePath(".claude.json"),
			`{"mcpServers":{"plugin:acme:tools":{"url":"https://user.example.com/mcp"}}}`)

		// The user table outranks the plugin anyway, so this only proves the malformed
		// file did not take the resolution down with it.
		assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj"))),
			"https://user.example.com/mcp")
	})

	t.Run("absent registry", func(t *testing.T) {
		f := newFixture(t, "darwin")
		res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj")))
		assertUnresolved(t, res, "was not found")
		for _, step := range res.Trace {
			if strings.Contains(step.Path, "plugins") {
				t.Fatalf("consulted a plugin source with no plugins installed:\n%s", resolveTrace(res))
			}
		}
	})
}

// TestClaudeCodePluginOrdinaryNameCostsNothing is the hot-path guard. Almost every
// call is not a plugin call, and one must not read a plugin file or grow a trace step
// on a machine full of plugins.
func TestClaudeCodePluginOrdinaryNameCostsNothing(t *testing.T) {
	f := newFixture(t, "darwin")
	project := f.path("proj")
	root := f.installPlugin("acme@market")
	f.write(filepath.Join(root, ".mcp.json"), `{"tools":{"url":"https://plugin.example.com/mcp"}}`)
	skill := f.mkdir(f.homePath(".claude", "skills", "notes"))
	f.write(filepath.Join(skill, ".mcp.json"), `{"notes":{"url":"https://skill.example.com/mcp"}}`)

	res := Resolve(f.Env, claudeCodeReq("myserver", project))
	want := []string{
		f.machinePath(claudeManagedMCPDarwin),
		filepath.Join(project, ".mcp.json"),
		f.homePath(".claude.json"),
	}
	if got := consultedPaths(res); !slices.Equal(got, want) {
		t.Fatalf("consulted\n%v\nwant\n%v\n%s", got, want, resolveTrace(res))
	}
}

// TestClaudeCodePluginUnrelatedPluginsAreNotRead covers the per-plugin prefix filter:
// a plugin that cannot own the reported name contributes neither I/O nor a step.
func TestClaudeCodePluginUnrelatedPluginsAreNotRead(t *testing.T) {
	f := newFixture(t, "darwin")
	acme := f.homePath(".claude", "plugins", "cache", "market", "acme", "1.0.0")
	other := f.homePath(".claude", "plugins", "cache", "market", "other", "1.0.0")
	f.mkdir(acme)
	f.mkdir(other)
	f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), fmt.Sprintf(
		`{"plugins":{"acme@market":[{"scope":"user","installPath":%s}],"other@market":[{"scope":"user","installPath":%s}]}}`,
		quote(acme), quote(other)))
	f.write(filepath.Join(acme, ".mcp.json"), `{"tools":{"url":"https://acme.example.com/mcp"}}`)
	f.write(filepath.Join(other, ".mcp.json"), `{"tools":{"url":"https://other.example.com/mcp"}}`)

	res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj")))
	assertURL(t, res, "https://acme.example.com/mcp")
	for _, step := range res.Trace {
		if strings.HasPrefix(step.Path, other) {
			t.Fatalf("consulted an unrelated plugin:\n%s", resolveTrace(res))
		}
	}
}

// TestClaudeCodePluginUnenumerableSourcesDeny covers the rule a plugin source we
// cannot enumerate answers to. Every case here has a plugin that resolves perfectly
// well on its own, and every case denies anyway: with the plugin set unknown we
// cannot say that plugin is the only one folding to the name.
func TestClaudeCodePluginUnenumerableSourcesDeny(t *testing.T) {
	// working sets up a fixture holding one plugin that resolves, so each case below
	// is only about the source that could not be read.
	working := func(t *testing.T) *fixture {
		f := newFixture(t, "darwin")
		root := f.installPlugin("acme@market")
		f.write(filepath.Join(root, ".mcp.json"), `{"tools":{"url":"https://plugin.example.com/mcp"}}`)
		assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj"))), "https://plugin.example.com/mcp")
		return f
	}

	t.Run("unreadable registry", func(t *testing.T) {
		f := working(t)
		f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), `{"plugins":`)
		assertUnresolved(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj"))),
			"the plugin registry at")
	})

	t.Run("too many installations", func(t *testing.T) {
		f := working(t)
		entries := make([]string, 0, maxPluginInstalls+1)
		for i := range maxPluginInstalls + 1 {
			root := f.mkdir(f.homePath("plugins", fmt.Sprintf("p%04d", i)))
			f.write(filepath.Join(root, ".mcp.json"), fmt.Sprintf(`{"tools":{"url":"https://p%04d.example.com/mcp"}}`, i))
			entries = append(entries, fmt.Sprintf(`%s:[{"scope":"user","installPath":%s}]`,
				quote(fmt.Sprintf("p%04d@market", i)), quote(root)))
		}
		f.write(f.homePath(".claude", "plugins", "installed_plugins.json"),
			`{"plugins":{`+strings.Join(entries, ",")+`}}`)

		// Even p0000, which sorts first and is well inside the bound, denies.
		assertUnresolved(t, Resolve(f.Env, claudeCodeReq("plugin_p0000_tools", f.path("proj"))),
			fmt.Sprintf("more than %d plugin installations are recorded", maxPluginInstalls))
	})

	t.Run("too many skill directories", func(t *testing.T) {
		f := working(t)
		for i := range maxSkillDirEntries + 1 {
			f.mkdir(f.homePath(".claude", "skills", fmt.Sprintf("s%04d", i)))
		}
		assertUnresolved(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj"))),
			fmt.Sprintf("holds more than %d entries", maxSkillDirEntries))
	})

	t.Run("unreadable skill directory", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root reads a directory regardless of its mode")
		}
		f := working(t)
		skills := f.mkdir(f.homePath(".claude", "skills"))
		if err := os.Chmod(skills, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(skills, 0o755) })

		assertUnresolved(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj"))),
			"could not be read")
	})

	t.Run("skills path is a file", func(t *testing.T) {
		// Not a gap: nothing is there to enumerate, so this must keep resolving.
		f := working(t)
		f.write(f.homePath(".claude", "skills"), "not a directory")
		assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj"))), "https://plugin.example.com/mcp")
	})

	t.Run("project skills path is a file", func(t *testing.T) {
		// The ancestor probe must walk past this rather than handing the enumerator a
		// path whose parent is a file.
		f := working(t)
		project := f.path("proj")
		f.write(filepath.Join(project, ".claude"), "not a directory")
		assertURL(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", project)), "https://plugin.example.com/mcp")
	})
}

// TestClaudeCodePluginGapSurvivesAnAmbiguousToolName covers the one way a gap could
// be papered over. A tool name that divides more than one way resolves once per
// reading, and a reading denied by a gap must not be discarded in favor of a sibling
// reading that happened to resolve — that would put the gap back where it started.
func TestClaudeCodePluginGapSurvivesAnAmbiguousToolName(t *testing.T) {
	f := newFixture(t, "darwin")
	project := f.path("proj")
	// "mcp__plugin__x__echo" divides as ("plugin", "x__echo") and ("plugin__x",
	// "echo"). Only the second is plugin-namespaced, and only the first is configured.
	f.write(f.homePath(".claude.json"), `{"mcpServers":{"plugin":{"url":"https://user.example.com/sse"}}}`)
	f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), `{"plugins":`)

	call, err := normalizeCall(f.Env, localagent.ClaudeCode, EventPreToolUse, mustJSON(map[string]any{
		"tool_name": "mcp__plugin__x__echo",
		"cwd":       project,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !call.Request.Unresolved {
		t.Fatalf("the configured reading answered for one whose plugins could not be enumerated: %+v", call.Request)
	}
	if !strings.Contains(call.Request.UnresolvedReason, "more than one way") {
		t.Fatalf("reason = %q, want the ambiguous-tool-name denial", call.Request.UnresolvedReason)
	}
}

// TestClaudeCodePluginGapDoesNotReachOrdinaryNames covers the blast radius of a gap.
// It denies plugin names, which is the whole point, and must not touch anything else
// — an unreadable skills directory is not a reason to deny every tool call on the box.
func TestClaudeCodePluginGapDoesNotReachOrdinaryNames(t *testing.T) {
	f := newFixture(t, "darwin")
	f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), `{"plugins":`)
	f.write(f.homePath(".claude.json"), `{"mcpServers":{"myserver":{"url":"https://global.example.com/sse"}}}`)

	assertURL(t, Resolve(f.Env, claudeCodeReq("myserver", f.path("proj"))), "https://global.example.com/sse")
	assertUnresolved(t, Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj"))), "the plugin registry at")
}

// TestClaudeCodePluginGapYieldsToTheManagedLockdown covers the one thing that outranks
// a gap. The managed set stops plugin servers from running at all, so a plugin tree we
// could not enumerate cannot change the answer, and the reason should say the thing
// that actually decided it.
func TestClaudeCodePluginGapYieldsToTheManagedLockdown(t *testing.T) {
	f := newFixture(t, "darwin")
	f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), `{"plugins":`)
	f.write(f.machinePath(claudeManagedMCPDarwin), `{"mcpServers":{"github":{"url":"https://managed.example.com/sse"}}}`)

	res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj")))
	assertUnresolved(t, res, "managed MCP configuration, which cannot be overridden")
	if len(res.Trace) != 1 {
		t.Fatalf("resolution continued past the managed config:\n%s", resolveTrace(res))
	}
}

// TestClaudeCodePluginGapIsTraced covers the trace side. A denial whose cause is a
// source rather than the call has to name that source, or the reason is unanswerable.
func TestClaudeCodePluginGapIsTraced(t *testing.T) {
	f := newFixture(t, "darwin")
	registry := f.write(f.homePath(".claude", "plugins", "installed_plugins.json"), `{"plugins":`)

	res := Resolve(f.Env, claudeCodeReq("plugin_acme_tools", f.path("proj")))
	last := res.Trace[len(res.Trace)-1]
	if last.Path != registry || last.Matched || !last.Exists {
		t.Fatalf("expected a final unmatched step naming %s:\n%s", registry, resolveTrace(res))
	}
	if !strings.Contains(last.Note, "could not be read") {
		t.Fatalf("trace note = %q, want it to name the cause\n%s", last.Note, resolveTrace(res))
	}
}

// TestClaudeCodePluginFileOrder pins the full source order for a plugin name, the
// way TestClaudeCodeFileOrder does for an ordinary one. The order is the contract:
// the plugin's own sources come last, and its manifest comes before its .mcp.json.
func TestClaudeCodePluginFileOrder(t *testing.T) {
	f := newFixture(t, "darwin")
	project := f.path("proj")
	root := f.installPlugin("acme@market")
	f.write(filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"acme","mcpServers":{"other":{"url":"https://other.example.com/mcp"}}}`)
	f.write(filepath.Join(root, ".mcp.json"), `{"other":{"url":"https://other.example.com/mcp"}}`)
	f.write(f.homePath(".claude.json"), `{"projects":{`+quote(project)+`:{"mcpServers":{}}}}`)

	res := Resolve(f.Env, claudeCodeReq("plugin_acme_missing", project))
	assertUnresolved(t, res, "was not found")

	want := []string{
		f.machinePath(claudeManagedMCPDarwin),
		filepath.Join(project, ".mcp.json"),
		f.homePath(".claude.json"), // projects[<project>]
		f.homePath(".claude.json"), // mcpServers
		filepath.Join(root, ".claude-plugin", "plugin.json"),
		filepath.Join(root, ".mcp.json"),
	}
	if got := consultedPaths(res); !slices.Equal(got, want) {
		t.Fatalf("consulted\n%v\nwant\n%v\n%s", got, want, resolveTrace(res))
	}
}

// TestClaudeCodePluginToolCallEndToEnd runs a real hook payload through the same path
// production uses, so the tool half and the reported name are pinned together.
func TestClaudeCodePluginToolCallEndToEnd(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("context7@claude-plugins-official")
	f.write(filepath.Join(root, ".mcp.json"),
		`{"context7":{"command":"npx","args":["-y","@upstash/context7-mcp"]}}`)

	call, err := normalizeCall(f.Env, localagent.ClaudeCode, EventPreToolUse, mustJSON(map[string]any{
		"tool_name": "mcp__plugin_context7_context7__query-docs",
		"cwd":       f.path("proj"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if call.Request.Unresolved {
		t.Fatalf("unresolved: %s", call.Request.UnresolvedReason)
	}
	if call.Request.Tool != "query-docs" {
		t.Fatalf("tool = %q, want %q", call.Request.Tool, "query-docs")
	}
	if call.Request.ServerName != "plugin:context7:context7" {
		t.Fatalf("server name = %q, want %q", call.Request.ServerName, "plugin:context7:context7")
	}
	pkg := call.Request.Server.Package
	if pkg == nil || pkg.Name != "@upstash/context7-mcp" {
		t.Fatalf("package = %+v, want @upstash/context7-mcp", pkg)
	}
}

// TestClaudeCodePluginBareMapNamedMcpServers pins the one place the lax plugin parser
// misreads a file. The agent takes `doc.mcpServers || doc`, so a bare-map server
// named mcpServers is read as the wrapper — matching it is the point.
func TestClaudeCodePluginBareMapNamedMcpServers(t *testing.T) {
	f := newFixture(t, "darwin")
	root := f.installPlugin("acme@market")
	f.write(filepath.Join(root, ".mcp.json"),
		`{"mcpServers":{"url":"https://shadowed.example.com/mcp"}}`)

	// Read as a wrapper holding one server named "url", not as a server named
	// "mcpServers".
	assertUnresolved(t, Resolve(f.Env, claudeCodeReq("plugin_acme_mcpServers", f.path("proj"))), "was not found")
	res := Resolve(f.Env, claudeCodeReq("plugin_acme_url", f.path("proj")))
	assertUnresolved(t, res, "neither a URL nor a command")
	if res.ServerName != "plugin:acme:url" {
		t.Fatalf("server name = %q\n%s", res.ServerName, resolveTrace(res))
	}
}
