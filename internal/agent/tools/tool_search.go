package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"charm.land/fantasy"
)

const ToolSearchToolName = "tool_search"

// DeferredParamsHint marks a stub description as missing its parameter schema.
// It is deliberately terse: it is repeated once per deferred tool, so a full
// sentence here would cost more than some tools' schemas do. The convention is
// explained once, in the tool_search tool's own description.
const DeferredParamsHint = "[params: tool_search]"

// deferredTool presents a tool by name and one-line summary while withholding
// its parameter schema, which is where nearly all the bytes live. The real
// schema is served on demand by the tool_search tool.
//
// The stub is deliberately immutable for the lifetime of the process: a
// "loaded" flag that swapped in the full schema after a search would change the
// tools block between turns, and since tools render before system and messages,
// that invalidates the entire prompt cache on every discovery. Serving schemas
// as tool *results* keeps them in message content, after every cache
// breakpoint, so discovery costs one round-trip instead of the whole prefix.
type deferredTool struct {
	inner   fantasy.AgentTool
	summary string
}

// NewDeferredTool wraps a tool so only its name and a one-line summary are sent.
func NewDeferredTool(inner fantasy.AgentTool) fantasy.AgentTool {
	return &deferredTool{
		inner:   inner,
		summary: summarize(inner.Info().Description),
	}
}

// WorthDeferring reports whether withholding a tool's schema actually shrinks
// the request. A tool with a terse description and one or two parameters can be
// smaller sent whole than as a stub, and deferring it would buy a round-trip for
// nothing.
func WorthDeferring(t fantasy.AgentTool) bool {
	full, err := json.Marshal(t.Info())
	if err != nil {
		return false
	}
	stub, err := json.Marshal(NewDeferredTool(t).Info())
	if err != nil {
		return false
	}
	return len(stub) < len(full)
}

func (d *deferredTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        d.inner.Info().Name,
		Description: d.summary + " " + DeferredParamsHint,
		// An empty object schema keeps the tool callable: the model supplies
		// arguments it learned from tool_search, and the MCP server remains the
		// authority on validating them.
		Parameters: map[string]any{},
		Required:   []string{},
		Parallel:   d.inner.Info().Parallel,
	}
}

// Run delegates to the real tool, but first catches the failure mode deferral
// creates: because the stub advertises no parameters, a model may call the tool
// optimistically without loading its schema. Rather than forwarding a call we
// know is malformed -- where the server's own error may not hint that a schema
// is available -- answer with the schema itself, so one round-trip fixes it
// regardless of how the upstream server words its errors.
//
// This makes calling optimistically a viable strategy: the model can skip
// tool_search when it is confident and still be corrected cheaply when wrong.
func (d *deferredTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if missing := d.missingRequired(call.Input); len(missing) > 0 {
		return fantasy.NewTextErrorResponse(d.schemaHint(missing)), nil
	}
	return d.inner.Run(ctx, call)
}

// missingRequired reports which of the tool's required parameters the call omits.
func (d *deferredTool) missingRequired(input string) []string {
	required := d.inner.Info().Required
	if len(required) == 0 {
		return nil
	}
	got := map[string]any{}
	if strings.TrimSpace(input) != "" {
		if err := json.Unmarshal([]byte(input), &got); err != nil {
			// Unparseable input is the server's to reject; only absent required
			// parameters are diagnosed here.
			return nil
		}
	}
	var missing []string
	for _, name := range required {
		v, ok := got[name]
		if !ok || v == nil || v == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// schemaHint is the corrective response: what was missing, plus the full schema.
func (d *deferredTool) schemaHint(missing []string) string {
	info := d.inner.Info()
	var b strings.Builder
	fmt.Fprintf(&b, "This call is missing required parameter(s): %s.\n\n",
		strings.Join(missing, ", "))
	b.WriteString("Here is the tool's full schema; retry the call with it. ")
	b.WriteString("You do not need to call tool_search for this tool.\n\n")
	payload, err := json.MarshalIndent(toolSearchHit{
		Name:        info.Name,
		Description: info.Description,
		Parameters:  info.Parameters,
		Required:    info.Required,
	}, "", "  ")
	if err != nil {
		fmt.Fprintf(&b, "required: %s", strings.Join(info.Required, ", "))
		return b.String()
	}
	b.Write(payload)
	return b.String()
}

func (d *deferredTool) ProviderOptions() fantasy.ProviderOptions {
	return d.inner.ProviderOptions()
}

func (d *deferredTool) SetProviderOptions(o fantasy.ProviderOptions) {
	d.inner.SetProviderOptions(o)
}

// Unwrap exposes the underlying tool so callers that need the real schema
// (notably tool_search) can reach it.
func (d *deferredTool) Unwrap() fantasy.AgentTool { return d.inner }

// summarize reduces a tool description to its first sentence, bounded, so the
// stub stays cheap while remaining recognizable.
func summarize(desc string) string {
	desc = strings.TrimSpace(strings.ReplaceAll(desc, "\n", " "))
	for strings.Contains(desc, "  ") {
		desc = strings.ReplaceAll(desc, "  ", " ")
	}
	if desc == "" {
		return "(no description provided)"
	}
	if i := strings.IndexAny(desc, ".!?"); i > 0 && i < 200 {
		desc = desc[:i+1]
	}
	// maxLen bounds the whole returned string, ellipsis included, so a caller can
	// budget stub size without accounting for the suffix separately.
	const maxLen = 200
	const ellipsis = "…"
	if len(desc) > maxLen {
		budget := maxLen - len(ellipsis)
		cut := strings.LastIndex(desc[:budget], " ")
		if cut <= 0 {
			cut = budget
		}
		desc = desc[:cut] + ellipsis
	}
	return desc
}

type ToolSearchParams struct {
	Query string   `json:"query" description:"What you want to do, in a few words (e.g. \"query prometheus metrics\", \"read a Jira issue\"). Matched against tool names and descriptions."`
	Names []string `json:"names,omitempty" description:"Optional. Exact tool names to load, when you already know them. Skips ranking."`
}

// toolSearchTool serves full schemas for deferred tools on demand.
type toolSearchTool struct {
	deferred        []fantasy.AgentTool // the wrapped tools, in stable order
	maxResults      int
	providerOptions fantasy.ProviderOptions
}

// NewToolSearchTool builds the discovery tool over a set of deferred tools.
// The slice is sorted by name so the tool's own description — which lists the
// servers it covers — is byte-stable across rebuilds, which the prompt cache
// requires.
func NewToolSearchTool(deferred []fantasy.AgentTool, maxResults int) fantasy.AgentTool {
	if maxResults <= 0 {
		maxResults = 5
	}
	sorted := make([]fantasy.AgentTool, len(deferred))
	copy(sorted, deferred)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Info().Name < sorted[j].Info().Name
	})
	return &toolSearchTool{deferred: sorted, maxResults: maxResults}
}

func (t *toolSearchTool) SetProviderOptions(o fantasy.ProviderOptions) { t.providerOptions = o }
func (t *toolSearchTool) ProviderOptions() fantasy.ProviderOptions     { return t.providerOptions }

func (t *toolSearchTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        ToolSearchToolName,
		Description: t.description(),
		Parameters: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "What you want to do, in a few words. Matched against tool names and descriptions.",
			},
			"names": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional. Exact tool names to load when you already know them.",
			},
		},
		Required: []string{},
	}
}

func (t *toolSearchTool) description() string {
	var b strings.Builder
	b.WriteString("Load the full parameter schema for a tool that was sent without one.\n\n")
	fmt.Fprintf(&b, "Tools marked %s in their description have no parameter schema in this request. ",
		DeferredParamsHint)
	fmt.Fprintf(&b, "There are %d of them", len(t.deferred))
	if servers := t.servers(); len(servers) > 0 {
		fmt.Fprintf(&b, ", from: %s", strings.Join(servers, ", "))
	}
	b.WriteString(".\n\nCall this first for any such tool, then call the tool itself. ")
	b.WriteString("Search by intent (\"query prometheus\", \"list alert rules\") or pass exact names in `names`. ")
	b.WriteString("The result gives each match's full description and JSON schema.")
	return b.String()
}

// servers lists the distinct MCP server prefixes covered, in sorted order.
func (t *toolSearchTool) servers() []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range t.deferred {
		name := d.Info().Name
		rest, ok := strings.CutPrefix(name, "mcp__")
		if !ok {
			continue
		}
		server, _, _ := strings.Cut(rest, "__")
		if server != "" && !seen[server] {
			seen[server] = true
			out = append(out, server)
		}
	}
	sort.Strings(out)
	return out
}

type toolSearchHit struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Required    []string       `json:"required,omitempty"`
}

func (t *toolSearchTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var params ToolSearchParams
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &params); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid parameters: %s", err)), nil
		}
	}
	if strings.TrimSpace(params.Query) == "" && len(params.Names) == 0 {
		return fantasy.NewTextErrorResponse(
			"provide a query describing what you want to do, or exact tool names to load"), nil
	}

	var hits []fantasy.AgentTool
	var unknown []string
	if len(params.Names) > 0 {
		byName := make(map[string]fantasy.AgentTool, len(t.deferred))
		for _, d := range t.deferred {
			byName[d.Info().Name] = d
		}
		for _, n := range params.Names {
			n = strings.TrimSpace(n)
			if d, ok := byName[n]; ok {
				hits = append(hits, d)
			} else {
				unknown = append(unknown, n)
			}
		}
	}
	if len(hits) == 0 {
		hits = t.rank(params.Query)
	}
	if len(hits) == 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"no tool matched %q. Available servers: %s",
			params.Query, strings.Join(t.servers(), ", "))), nil
	}

	out := struct {
		Matches []toolSearchHit `json:"matches"`
		Unknown []string        `json:"unknown_names,omitempty"`
		Note    string          `json:"note"`
	}{
		Unknown: unknown,
		Note:    "Call the tool by name with these parameters. Do not call tool_search again for the same tool.",
	}
	for _, d := range hits {
		info := d.Info()
		if u, ok := d.(interface{ Unwrap() fantasy.AgentTool }); ok {
			info = u.Unwrap().Info()
		}
		out.Matches = append(out.Matches, toolSearchHit{
			Name:        info.Name,
			Description: info.Description,
			Parameters:  info.Parameters,
			Required:    info.Required,
		})
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fantasy.NewTextErrorResponse("could not encode matches"), nil
	}
	return fantasy.NewTextResponse(string(b)), nil
}

// rank scores deferred tools against a free-text query. Scoring is a small
// lexical overlap heuristic rather than real BM25: with a few hundred tools the
// discriminating signal is almost entirely whether query terms appear in the
// tool name, and a heuristic keeps this dependency-free and deterministic.
func (t *toolSearchTool) rank(query string) []fantasy.AgentTool {
	terms := tokenize(query)
	if len(terms) == 0 {
		return nil
	}

	type scored struct {
		tool  fantasy.AgentTool
		score int
		name  string
	}
	var all []scored
	for _, d := range t.deferred {
		info := d.Info()
		full := info
		if u, ok := d.(interface{ Unwrap() fantasy.AgentTool }); ok {
			full = u.Unwrap().Info()
		}
		name := strings.ToLower(full.Name)
		desc := strings.ToLower(full.Description)
		score := 0
		for _, term := range terms {
			if strings.Contains(name, term) {
				score += 8 // a name hit is far more discriminating than a description hit
			}
			if strings.Contains(desc, term) {
				score += 2
			}
		}
		if score > 0 {
			all = append(all, scored{tool: d, score: score, name: full.Name})
		}
	}

	// Ties break on name so identical queries always return identical results.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].name < all[j].name
	})
	if len(all) > t.maxResults {
		all = all[:t.maxResults]
	}
	out := make([]fantasy.AgentTool, 0, len(all))
	for _, s := range all {
		out = append(out, s.tool)
	}
	return out
}

// toolSearchStopWords are terms too common in tool descriptions to discriminate.
var toolSearchStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "the": true, "to": true, "of": true,
	"for": true, "in": true, "on": true, "with": true, "from": true, "by": true,
	"is": true, "it": true, "this": true, "that": true, "tool": true, "use": true,
	"using": true, "get": true, "me": true, "my": true, "i": true, "want": true,
}

func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		if len(f) < 2 || toolSearchStopWords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}
