package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/astgrep"
	"github.com/charmbracelet/crush/internal/filepathext"
)

const AstGrepToolName = "AstGrep"

//go:embed astgrep.md
var astGrepDescription string

const astGrepMatchLimit = 50

// AstGrepParams is the input to the structural search tool.
type AstGrepParams struct {
	Pattern string `json:"pattern" description:"Structural pattern written as a code fragment with metavariable holes: $VAR captures one node, $$$VARS captures a sequence (e.g. call arguments), $_ matches one node without capturing. Example: '$A || $B'"`
	Path    string `json:"path,omitempty" description:"File or directory to search. Defaults to the working directory. Language is inferred from each file's extension."`
}

// AstGrepResponseMetadata reports the match count for the UI.
type AstGrepResponseMetadata struct {
	NumberOfMatches int  `json:"number_of_matches"`
	Truncated       bool `json:"truncated"`
}

// NewAstGrepTool builds the read-only structural code search tool. Patterns
// match tree-sitter syntax-tree shape rather than text, so results ignore
// formatting and never match inside strings or comments. Each match reports its
// enclosing symbol to help locate the right occurrence of a repeated pattern.
func NewAstGrepTool(workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AstGrepToolName,
		astGrepDescription,
		func(ctx context.Context, params AstGrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(params.Pattern) == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}

			target := cmp.Or(params.Path, ".")
			abs := filepathext.SmartJoin(workingDir, target)
			info, err := os.Stat(abs)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("path not found: %s", target)), nil
			}

			var files []string
			if info.IsDir() {
				files, _, err = globFiles(ctx, "**/*", abs, 5000)
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("error listing files: %v", err)), nil
				}
				sort.Strings(files)
			} else {
				files = []string{abs}
			}

			out, total, truncated, compiledAny := astGrepSearchFiles(ctx, params.Pattern, files, workingDir)
			if !compiledAny {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"could not compile the pattern for any matched file. Check the pattern parses as %s code, or point path at files of the intended language.",
					filepath.Ext(target))), nil
			}
			if total == 0 {
				return fantasy.NewTextResponse("No matches found."), nil
			}
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(out),
				AstGrepResponseMetadata{NumberOfMatches: total, Truncated: truncated},
			), nil
		},
	)
}

// astGrepSearchFiles searches every file with the pattern (recompiled per file
// extension), rendering grouped, capped results. compiledAny is false when the
// pattern could not be compiled for any file's language.
func astGrepSearchFiles(ctx context.Context, pattern string, files []string, workingDir string) (out string, total int, truncated, compiledAny bool) {
	cache := map[string]*astgrep.Pattern{}
	compile := func(path string) (*astgrep.Pattern, bool) {
		ext := filepath.Ext(path)
		if p, seen := cache[ext]; seen {
			return p, p != nil
		}
		p, err := astgrep.Compile(pattern, path)
		if err != nil {
			cache[ext] = nil
			return nil, false
		}
		cache[ext] = p
		return p, true
	}

	var b strings.Builder
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		p, ok := compile(f)
		if !ok {
			continue
		}
		compiledAny = true
		data, err := os.ReadFile(f)
		if err != nil || len(data) > MaxViewSize || !utf8.Valid(data) {
			continue
		}
		matches, err := p.Search(string(data))
		if err != nil || len(matches) == 0 {
			continue
		}

		rel := f
		if r, err := filepath.Rel(workingDir, f); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		fmt.Fprintf(&b, "%s\n", filepath.ToSlash(rel))
		for _, m := range matches {
			if total >= astGrepMatchLimit {
				truncated = true
				break
			}
			writeAstMatch(&b, m)
			total++
		}
		b.WriteByte('\n')
		if truncated {
			break
		}
	}

	res := strings.TrimRight(b.String(), "\n")
	if truncated {
		res += fmt.Sprintf("\n\n(Showing the first %d matches. Narrow the pattern or path to see the rest.)", astGrepMatchLimit)
	}
	return res, total, truncated, compiledAny
}

// writeAstMatch renders one match: location, enclosing symbol, the first line of
// the matched text, and any metavariable bindings.
func writeAstMatch(b *strings.Builder, m astgrep.Match) {
	first, _, _ := strings.Cut(m.Text, "\n")
	first = strings.TrimSpace(first)
	if len(first) > 120 {
		first = first[:120] + "…"
	}
	loc := fmt.Sprintf("  %d:%d", m.StartLine, m.StartCol)
	if m.Enclosing != "" {
		fmt.Fprintf(b, "%s  (in %s)  %s\n", loc, m.Enclosing, first)
	} else {
		fmt.Fprintf(b, "%s  %s\n", loc, first)
	}
	if len(m.Bindings) > 0 {
		keys := make([]string, 0, len(m.Bindings))
		for k := range m.Bindings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			v, _, _ := strings.Cut(m.Bindings[k], "\n")
			if len(v) > 40 {
				v = v[:40] + "…"
			}
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		fmt.Fprintf(b, "        meta: %s\n", strings.Join(parts, ", "))
	}
}
