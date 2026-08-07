package enforce

import (
	"context"
	"slices"
	"testing"
)

// fixedScope builds a scope serving set, for exercising the engine without a
// filesystem.
func fixedScope(name string, set serverSet) scope {
	return scope{path: name, key: mcpServersKey, load: fixedServers(set, loadOK)}
}

func urlSet(name, url string) serverSet {
	return serverSet{name: {URL: url}}
}

func exactly(name string) lookup {
	return lookup{names: []string{name}}
}

func TestResolveScopesMultipleSourcesAreAmbiguous(t *testing.T) {
	scopes := []scope{
		fixedScope("first", urlSet("linear", "https://first.example.com/sse")),
		fixedScope("second", urlSet("linear", "https://second.example.com/sse")),
	}

	if _, out := resolveScopes(t.Context(), scopes, exactly("linear"), &tracer{}); out != outcomeAmbiguous {
		t.Fatalf("outcome = %v, want outcomeAmbiguous", out)
	}
}

func TestResolveScopesChecksEveryApplicableSource(t *testing.T) {
	loaded := false
	scopes := []scope{
		fixedScope("first", urlSet("linear", "https://first.example.com/sse")),
		{path: "second", key: mcpServersKey, load: func(context.Context) (serverSet, loadResult) {
			loaded = true
			return urlSet("github", "https://second.example.com/sse"), loadOK
		}},
	}

	if _, out := resolveScopes(t.Context(), scopes, exactly("linear"), &tracer{}); out != outcomeFound {
		t.Fatalf("outcome = %v, want outcomeFound", out)
	}
	if !loaded {
		t.Fatal("a later applicable scope was not loaded after the first one matched")
	}
}

func TestResolveScopesMiss(t *testing.T) {
	scopes := []scope{fixedScope("only", urlSet("github", "https://github.example.com/sse"))}

	if _, out := resolveScopes(t.Context(), scopes, exactly("linear"), &tracer{}); out != outcomeMiss {
		t.Fatalf("outcome = %v, want outcomeMiss", out)
	}
}

func TestResolveScopesDifferentDeclarationsAreAmbiguous(t *testing.T) {
	scopes := []scope{
		fixedScope("a", urlSet("linear", "https://a.example.com/sse")),
		fixedScope("b", urlSet("linear", "https://b.example.com/sse")),
	}

	if _, out := resolveScopes(t.Context(), scopes, exactly("linear"), &tracer{}); out != outcomeAmbiguous {
		t.Fatalf("outcome = %v, want outcomeAmbiguous", out)
	}
}

func TestResolveScopesIdenticalDeclarationsAreAmbiguous(t *testing.T) {
	entry := mcpEntry{Command: "npx", Args: []string{"-y", "linear-mcp"}}
	scopes := []scope{
		fixedScope("a", serverSet{"linear": entry}),
		fixedScope("b", serverSet{"linear": entry}),
	}

	if _, out := resolveScopes(t.Context(), scopes, exactly("linear"), &tracer{}); out != outcomeAmbiguous {
		t.Fatalf("outcome = %v, want outcomeAmbiguous", out)
	}
}

func TestResolveScopesDifferentEnvironmentIsAmbiguous(t *testing.T) {
	base := mcpEntry{
		Command: "npx",
		Args:    []string{"-y", "linear-mcp"},
		Environment: map[string]string{
			"LINEAR_TOKEN": "project-token",
		},
	}
	shadow := base
	shadow.Environment = map[string]string{"LINEAR_TOKEN": "user-token"}
	scopes := []scope{
		fixedScope("project", serverSet{"linear": base}),
		fixedScope("user", serverSet{"linear": shadow}),
	}

	if _, out := resolveScopes(t.Context(), scopes, exactly("linear"), &tracer{}); out != outcomeAmbiguous {
		t.Fatalf("outcome = %v, want outcomeAmbiguous", out)
	}
}

func TestResolveScopesRecordsEveryMatch(t *testing.T) {
	scopes := []scope{
		fixedScope("a", urlSet("linear", "https://a.example.com/sse")),
		fixedScope("b", urlSet("linear", "https://b.example.com/sse")),
	}

	tr := &tracer{}
	resolveScopes(t.Context(), scopes, exactly("linear"), tr)

	var matched []string
	for _, step := range tr.steps {
		if step.Matched {
			matched = append(matched, step.Path)
		}
	}
	if !slices.Equal(matched, []string{"a", "b"}) {
		t.Fatalf("matched steps = %v, want both scopes", matched)
	}
}

func TestResolveScopesClosedScope(t *testing.T) {
	managed := fixedScope("managed", urlSet("github", "https://managed.example.com/sse"))
	managed.closed = true
	scopes := []scope{managed, fixedScope("user", urlSet("linear", "https://user.example.com/sse"))}

	if _, out := resolveScopes(t.Context(), scopes, exactly("linear"), &tracer{}); out != outcomeClosed {
		t.Fatalf("outcome = %v, want outcomeClosed", out)
	}
}

func TestResolveScopesClosedMatchSuppressesLowerDeclaration(t *testing.T) {
	loaded := false
	managed := fixedScope("managed", urlSet("linear", "https://managed.example.com/sse"))
	managed.closed = true
	scopes := []scope{
		managed,
		{path: "user", key: mcpServersKey, load: func(context.Context) (serverSet, loadResult) {
			loaded = true
			return urlSet("linear", "https://user.example.com/sse"), loadOK
		}},
	}

	m, out := resolveScopes(t.Context(), scopes, exactly("linear"), &tracer{})
	if out != outcomeFound || m.entry.URL != "https://managed.example.com/sse" {
		t.Fatalf("outcome = %v, match = %+v", out, m)
	}
	if loaded {
		t.Fatal("a source suppressed by the closed scope was loaded")
	}
}

func TestResolveScopesClosedScopeThatIsAbsent(t *testing.T) {
	scopes := []scope{
		{path: "managed", key: mcpServersKey, closed: true, load: fixedServers(nil, loadAbsent)},
		fixedScope("user", urlSet("linear", "https://user.example.com/sse")),
	}

	m, out := resolveScopes(t.Context(), scopes, exactly("linear"), &tracer{})
	if out != outcomeFound {
		t.Fatalf("outcome = %v, want outcomeFound", out)
	}
	if m.entry.URL != "https://user.example.com/sse" {
		t.Fatalf("URL = %q", m.entry.URL)
	}
}

func TestMatchNamesReturnsExactAndFormMatchesAcrossTheLadder(t *testing.T) {
	set := serverSet{
		"user-linear": {URL: "https://prefixed.example.com/sse"},
		"user.linear": {URL: "https://folded.example.com/sse"},
	}

	matches := matchNames(lookup{names: []string{"user-linear", "user_linear"}, form: formCodex}, set)
	if len(matches) != 2 {
		t.Fatalf("matches = %+v, want two", matches)
	}
	keys := []string{matches[0].key, matches[1].key}
	if !slices.Equal(keys, []string{"user-linear", "user.linear"}) {
		t.Fatalf("matched keys = %v", keys)
	}
}

func TestMatchNamesFormMatchesForward(t *testing.T) {
	set := serverSet{"my-linear": {URL: "https://linear.example.com/sse"}}

	if matches := matchNames(exactly("my_linear"), set); len(matches) != 0 {
		t.Fatal("matched without a form; only an exact key may match then")
	}
	matches := matchNames(lookup{names: []string{"my_linear"}, form: formCodex}, set)
	if len(matches) != 1 || matches[0].key != "my-linear" {
		t.Fatalf("matches = %+v, want the configuration key as written", matches)
	}

	// Not the reverse: the report is never folded.
	folded := serverSet{"my_linear": {URL: "https://linear.example.com/sse"}}
	if matches := matchNames(lookup{names: []string{"my-linear"}, form: formCodex}, folded); len(matches) != 0 {
		t.Fatal("matched backwards; the reported name must not be transformed")
	}
}

func TestMatchNamesFormReturnsEveryCollisionDeterministically(t *testing.T) {
	set := serverSet{
		"my-server": {URL: "https://hyphen.example.com/sse"},
		"my.server": {URL: "https://dot.example.com/sse"},
	}
	names := lookup{names: []string{"my_server"}, form: formCodex}

	matches := matchNames(names, set)
	if len(matches) != 2 {
		t.Fatalf("matches = %+v, want two", matches)
	}
	keys := []string{matches[0].key, matches[1].key}
	if !slices.Equal(keys, []string{"my-server", "my.server"}) {
		t.Fatalf("matched keys = %v, want sorted collisions", keys)
	}
	for range 20 {
		again := matchNames(names, set)
		if !slices.Equal([]string{again[0].key, again[1].key}, keys) {
			t.Fatalf("matched keys changed from %v to %+v", keys, again)
		}
	}
}

func TestDecodeServersKeepsMalformedSiblings(t *testing.T) {
	f := newFixture(t, "darwin")
	path := f.write(f.path("mcp.json"), `{"mcpServers":{
		"broken": ["not", "an", "object"],
		"linear": {"url":"https://linear.example.com/sse"}
	}}`)

	set, res := jsonServers(newConfigLoader(), path)(t.Context())
	if res != loadOK {
		t.Fatalf("load = %v, want loadOK", res)
	}
	if set["linear"].URL != "https://linear.example.com/sse" {
		t.Fatalf("the healthy sibling did not decode: %+v", set)
	}
	entry, ok := set["broken"]
	if !ok {
		t.Fatal("the malformed entry went missing")
	}
	if entry.URL != "" || entry.Command != "" || len(entry.Args) != 0 || len(entry.Environment) != 0 {
		t.Fatalf("malformed entry = %+v, want the zero entry", entry)
	}
}
