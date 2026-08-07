package enforce

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/obot-platform/obot-sentry/pkg/localagent"
)

const (
	mcpServersKey   = "mcpServers"
	codexServersKey = "mcp_servers"
)

// serverSet is a decoded MCP servers table, keyed as its file spells the names.
type serverSet map[string]mcpEntry

type cachedServerSet struct {
	set serverSet
	res loadResult
}

// scope is one place an agent can declare an MCP server.
type scope struct {
	// path and key name the source, for the trace.
	path string
	key  string
	// closed marks a scope whose server set the agent treats as exclusive, so a
	// readable source ends resolution instead of consulting lower scopes.
	closed bool
	load   func(context.Context) (serverSet, loadResult)
}

func (s scope) traceKey(matched string) string {
	if matched == "" {
		return s.key
	}
	return fmt.Sprintf("%s[%q]", s.key, matched)
}

// lookup is the ladder of names to try against a server set, most literal first.
type lookup struct {
	names []string
	form  namespaceForm
}

// namedMatch is one distinct key reached through a lookup.
type namedMatch[T any] struct {
	value T
	key   string
}

// matchNames runs a name ladder against the keys of m and returns every distinct
// key it can reach. Exact forms come first, followed by namespace forms in sorted
// key order, so traces remain deterministic. A key reached more than once is
// returned once: it is one declaration even if more than one lookup form names it.
func matchNames[T any](l lookup, m map[string]T) []namedMatch[T] {
	var matches []namedMatch[T]
	seen := make(map[string]struct{})
	for _, name := range l.names {
		if _, ok := seen[name]; ok {
			continue
		}
		if v, ok := m[name]; ok {
			matches = append(matches, namedMatch[T]{value: v, key: name})
			seen[name] = struct{}{}
		}
	}
	if l.form == nil {
		return matches
	}
	keys := slices.Sorted(maps.Keys(m))
	for _, name := range l.names {
		for _, k := range keys {
			if _, ok := seen[k]; ok {
				continue
			}
			if l.form(k) == name {
				matches = append(matches, namedMatch[T]{value: m[k], key: k})
				seen[k] = struct{}{}
			}
		}
	}
	return matches
}

// note describes a match whose key is not the name the call reported.
func (l lookup) note(matched string) string {
	if len(l.names) > 0 && matched == l.names[0] {
		return ""
	}
	return fmt.Sprintf("matched as %q", matched)
}

// outcome is what walking a scope list concluded.
type outcome int

const (
	// outcomeMiss means no scope declared the name.
	outcomeMiss outcome = iota
	// outcomeFound means one identity was established.
	outcomeFound
	// outcomeClosed means a closed scope ended resolution without a match.
	outcomeClosed
	// outcomeAmbiguous means more than one server declaration matched.
	outcomeAmbiguous
)

// match is a server declaration found in some scope.
type match struct {
	key   string
	entry mcpEntry
}

// resolveScopes walks every applicable scope and returns a declaration only when
// exactly one matches. Source order is diagnostic, not precedence: a second match
// is ambiguous even when it repeats the first definition byte-for-byte.
//
// A readable closed scope is the one exception. Its server set is exclusive, so
// lower sources cannot have governed the call and are not consulted.
func resolveScopes(ctx context.Context, scopes []scope, names lookup, tr *tracer) (match, outcome) {
	var found []match
	for _, s := range scopes {
		set, res := s.load(ctx)
		if res != loadOK {
			tr.miss(s.path, s.traceKey(""), res)
			continue
		}
		matches := matchNames(names, set)
		if len(matches) == 0 {
			tr.miss(s.path, s.traceKey(""), res)
		} else {
			for _, m := range matches {
				tr.hit(s.path, s.traceKey(m.key), names.note(m.key))
				found = append(found, match{key: m.key, entry: m.value})
			}
		}
		if s.closed {
			switch len(found) {
			case 0:
				return match{}, outcomeClosed
			case 1:
				return found[0], outcomeFound
			default:
				return match{}, outcomeAmbiguous
			}
		}
	}
	switch len(found) {
	case 0:
		return match{}, outcomeMiss
	case 1:
		return found[0], outcomeFound
	default:
		return match{}, outcomeAmbiguous
	}
}

func ambiguous(agent localagent.Agent, name string) Resolution {
	return unresolved(name, fmt.Sprintf(
		"more than one %s MCP server could match %q, so the hook cannot tell which one ran; rename or remove one of them",
		agent.DisplayName(), name))
}

// ambiguousToolName reports a tool name that divides into a server and a tool in
// more than one way, with more than one reading naming a configured server.
//
// The sibling of ambiguous(): that one is two scopes claiming one name, this one is
// one name claiming two servers. Both end the same way, because the honest answer is
// that the hook cannot say which server ran.
func ambiguousToolName(agent localagent.Agent, reported string, candidates []string) Resolution {
	slices.Sort(candidates)
	return unresolved(reported, fmt.Sprintf(
		"the tool name divides into an MCP server and a tool in more than one way, and %s configuration declares more than one of the resulting servers (%s), so the hook cannot tell which one ran; rename one of them",
		agent.DisplayName(), strings.Join(candidates, ", ")))
}

func jsonServers(loader *configLoader, path string) func(context.Context) (serverSet, loadResult) {
	return func(ctx context.Context) (serverSet, loadResult) {
		if ctx.Err() != nil {
			return nil, loadUnusable
		}
		if cached, ok := loader.jsonServerSets[path]; ok {
			return cached.set, cached.res
		}
		var doc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		res := loader.loadJSON(ctx, path, &doc)
		if ctx.Err() != nil {
			return nil, loadUnusable
		}
		if res != loadOK {
			loader.jsonServerSets[path] = cachedServerSet{res: res}
			return nil, res
		}
		set := decodeServers(doc.MCPServers)
		loader.jsonServerSets[path] = cachedServerSet{set: set, res: loadOK}
		return set, loadOK
	}
}

func codexServers(loader *configLoader, path string) func(context.Context) (serverSet, loadResult) {
	return func(ctx context.Context) (serverSet, loadResult) {
		if ctx.Err() != nil {
			return nil, loadUnusable
		}
		if cached, ok := loader.codexServerSets[path]; ok {
			return cached.set, cached.res
		}
		var doc struct {
			MCPServers serverSet `toml:"mcp_servers"`
		}
		res := loader.loadTOML(ctx, path, &doc)
		if ctx.Err() != nil {
			return nil, loadUnusable
		}
		if res != loadOK {
			loader.codexServerSets[path] = cachedServerSet{res: res}
			return nil, res
		}
		loader.codexServerSets[path] = cachedServerSet{set: doc.MCPServers, res: loadOK}
		return doc.MCPServers, loadOK
	}
}

// fixedServers serves an already-decoded table, for scopes that share one file.
func fixedServers(set serverSet, res loadResult) func(context.Context) (serverSet, loadResult) {
	return func(context.Context) (serverSet, loadResult) { return set, res }
}

// decodeServers decodes each entry on its own so one malformed sibling cannot cost
// the whole table, and with it every other server configured in it. A key that
// fails to decode stays present with a zero entry: it names a configured server
// that identifies nothing, which is a match rather than an invitation for another
// source to answer for it.
func decodeServers(raw map[string]json.RawMessage) serverSet {
	set := make(serverSet, len(raw))
	for name, msg := range raw {
		var entry mcpEntry
		_ = json.Unmarshal(msg, &entry)
		set[name] = entry
	}
	return set
}
