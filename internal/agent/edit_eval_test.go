package agent

// Edit-tool evaluation harness.
//
// This is a live, on-demand benchmark that measures how reliably a model makes
// a requested single-file edit in "string" mode (exact find/replace Edit +
// MultiEdit) versus "hashline" mode (line-anchored patches). It reuses the
// oh-my-pi typescript-edit-benchmark fixtures vendored under
// testdata/edit_evals/ (input file + expected file + prompt + metadata).
//
// It is skipped unless CRUSH_EDIT_EVAL is set, because it makes real model
// calls. By default it drives Claude Haiku 4.5 through the local Claude Code
// subscription (~/.claude/.credentials.json); no API key is required.
//
// Env knobs:
//   CRUSH_EDIT_EVAL=1        enable the eval (required)
//   CRUSH_EDIT_EVAL_LIMIT=N  only run the first N fixtures (0 = all 80)
//   CRUSH_EDIT_EVAL_MODES=…  comma list of modes to run (default "string,hashline")
//   CRUSH_EDIT_EVAL_MCP_PAD= pad the tool list with the real captured MCP
//                            schemas: none|all|unused|<server,…>. See
//                            eval_toolpad_test.go — this is how the
//                            tool-surface A/B is run.
//   CRUSH_EDIT_EVAL_MAX_TOKENS= output cap (default 8000). Raise it on models
//                            where thinking is on by default, since max_tokens
//                            covers thinking plus the response and a tight cap
//                            truncates mid-edit and scores as a task failure.
//   CRUSH_EDIT_EVAL_RESULTS= append per-fixture outcomes as JSONL to this path,
//                            so two arms can be paired for a significance test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/hashline"
	"github.com/charmbracelet/crush/internal/hashline/tsblock"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

var evalModelID = envOr("CRUSH_EDIT_EVAL_MODEL", "claude-haiku-4-5-20251001")

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// haikuSubscriptionModel builds a subscription-authenticated model (Haiku by
// default, or CRUSH_EDIT_EVAL_MODEL) via the Claude Code subscription,
// mirroring coordinator.buildAnthropicProvider.
func haikuSubscriptionModel(t *testing.T) fantasy.LanguageModel {
	t.Helper()
	return subscriptionModel(t, evalModelID)
}

// subscriptionModel builds a subscription-authenticated model by id, mirroring
// coordinator.buildAnthropicProvider.
func subscriptionModel(t *testing.T, modelID string) fantasy.LanguageModel {
	t.Helper()
	base := &claudecode.AuthTransport{Base: http.DefaultTransport, Source: claudecode.DefaultSource()}
	httpClient := &http.Client{Transport: &ccSystemSplitTransport{base: base, injectIdentity: true}}
	prov, err := anthropic.New(anthropic.WithHTTPClient(httpClient))
	require.NoError(t, err)
	m, err := prov.LanguageModel(t.Context(), modelID)
	require.NoError(t, err)
	return m
}

// evalThinkingOptions returns provider options with an anthropic thinking budget
// from CRUSH_EDIT_EVAL_THINKING (token budget; 0/unset = thinking off).
func evalThinkingOptions() fantasy.ProviderOptions {
	budget := envInt("CRUSH_EDIT_EVAL_THINKING", 0)
	if budget <= 0 {
		return nil
	}
	return fantasy.ProviderOptions{
		"anthropic": &anthropic.ProviderOptions{
			Thinking: &anthropic.ThinkingProviderOption{BudgetTokens: int64(budget)},
		},
	}
}

type evalFixture struct {
	name       string
	fileName   string
	input      string
	expected   string
	prompt     string
	difficulty int
}

func loadEvalFixtures(t *testing.T) []evalFixture {
	t.Helper()
	root := filepath.Join("testdata", "edit_evals")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)

	var fixtures []evalFixture
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		inputFile := singleFile(t, filepath.Join(dir, "input"))
		fx := evalFixture{
			name:     e.Name(),
			fileName: filepath.Base(inputFile),
			input:    readFile(t, inputFile),
			expected: readFile(t, filepath.Join(dir, "expected", filepath.Base(inputFile))),
			prompt:   readFile(t, filepath.Join(dir, "prompt.md")),
		}
		if meta := readFile(t, filepath.Join(dir, "metadata.json")); meta != "" {
			var m struct {
				DifficultyScore int `json:"difficulty_score"`
			}
			_ = json.Unmarshal([]byte(meta), &m)
			fx.difficulty = m.DifficultyScore
		}
		fixtures = append(fixtures, fx)
	}
	sort.Slice(fixtures, func(i, j int) bool { return fixtures[i].name < fixtures[j].name })
	return fixtures
}

func singleFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if !e.IsDir() {
			return filepath.Join(dir, e.Name())
		}
	}
	t.Fatalf("no file in %s", dir)
	return ""
}

func readFile(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// evalTools returns the task tools plus any inert MCP padding, so an A/B run
// can measure whether carrying an unused tool surface costs task accuracy.
// Padding goes last, mirroring production where MCP tools sort after natives.
func evalTools(env fakeEnv, mode string, store *hashline.Store, pad []fantasy.AgentTool) []fantasy.AgentTool {
	return append(evalTaskTools(env, mode, store), pad...)
}

func evalTaskTools(env fakeEnv, mode string, store *hashline.Store) []fantasy.AgentTool {
	base := []fantasy.AgentTool{
		tools.NewLsTool(env.permissions, env.workingDir, config.ToolLs{}),
		tools.NewGrepTool(env.workingDir, config.ToolGrep{}),
		tools.NewWriteTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
	}
	if os.Getenv("CRUSH_EDIT_EVAL_ASTGREP") != "0" {
		base = append(base, tools.NewAstGrepTool(env.workingDir))
	}
	if mode == config.EditModeHashline {
		return append(
			base,
			tools.NewViewTool(nil, env.permissions, *env.filetracker, nil, nil, config.EditModeHashline, store, os.Getenv("CRUSH_EDIT_EVAL_SUMMARIZE") != "", 0, envInt("CRUSH_EDIT_EVAL_BUDGET", 400), env.workingDir),
			tools.NewHashlineEditTool(nil, env.permissions, env.history, *env.filetracker, store, tsblock.New(), env.workingDir, os.Getenv("CRUSH_EDIT_EVAL_LINT") != "0"),
		)
	}
	return append(
		base,
		tools.NewViewTool(nil, env.permissions, *env.filetracker, nil, nil, config.EditModeString, nil, os.Getenv("CRUSH_EDIT_EVAL_SUMMARIZE") != "", 0, envInt("CRUSH_EDIT_EVAL_BUDGET", 400), env.workingDir),
		tools.NewEditTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
		tools.NewMultiEditTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
	)
}

type evalResult struct {
	success    bool
	editCalls  int
	editErrors int
	tokens     int64
	runErr     error
	editInputs []string // raw edit tool-call inputs (for failure diagnostics)
	errSamples []string // error tool-result contents
	misfires   int64    // calls the model made into the inert MCP padding
}

func runEditTask(t *testing.T, model fantasy.LanguageModel, mode string, fx evalFixture, pad []fantasy.AgentTool, misfires *atomic.Int64) evalResult {
	env := testEnv(t)

	// Seed the working directory with the fixture input under its real name.
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, fx.fileName), []byte(fx.input), 0o644))

	sysPrompt, err := buildEvalPrompt(env, model)
	require.NoError(t, err)

	store := hashline.NewStore()
	before := misfires.Load()
	agent := testSessionAgent(env, model, model, sysPrompt, evalTools(env, mode, store, pad)...)

	session, err := env.sessions.Create(t.Context(), "eval")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	res, runErr := agent.Run(ctx, SessionAgentCall{
		Prompt:          fx.prompt,
		SessionID:       session.ID,
		MaxOutputTokens: int64(envInt("CRUSH_EDIT_EVAL_MAX_TOKENS", 8000)),
		NonInteractive:  true,
		ProviderOptions: evalThinkingOptions(),
	})

	out := evalResult{runErr: runErr}
	if res != nil {
		u := res.TotalUsage
		out.tokens = u.InputTokens + u.OutputTokens
		// Haiku 4.5 pricing (per 1M): in $1, out $5, cache-write $1.25, cache-read $0.10.
		cost := float64(u.InputTokens)*1e-6 + float64(u.OutputTokens)*5e-6 +
			float64(u.CacheCreationTokens)*1.25e-6 + float64(u.CacheReadTokens)*0.10e-6
		fmt.Printf("USAGE %s in=%d out=%d cacheW=%d cacheR=%d total=%d cost=%.5f\n",
			fx.name, u.InputTokens, u.OutputTokens, u.CacheCreationTokens, u.CacheReadTokens,
			u.InputTokens+u.OutputTokens+u.CacheCreationTokens+u.CacheReadTokens, cost)
	}

	got := readFile(t, filepath.Join(env.workingDir, fx.fileName))
	out.success = normalizeLF(got) == normalizeLF(fx.expected)

	// Count edit tool calls and edit errors from the transcript. The inert MCP
	// padding also returns error results, so classify each error by the name of
	// the call it answers — otherwise a misfire would inflate the edit-error
	// rate and look like an edit-tool regression.
	msgs, _ := env.messages.List(t.Context(), session.ID)
	editNames := map[string]bool{tools.EditToolName: true, tools.MultiEditToolName: true}
	callName := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls() {
			callName[tc.ID] = tc.Name
			if editNames[tc.Name] {
				out.editCalls++
				out.editInputs = append(out.editInputs, tc.Input)
			}
		}
	}
	for _, m := range msgs {
		for _, tr := range m.ToolResults() {
			if !tr.IsError || strings.HasPrefix(callName[tr.ToolCallID], "mcp__") {
				continue
			}
			out.editErrors++
			out.errSamples = append(out.errSamples, tr.Content)
		}
	}
	out.misfires = misfires.Load() - before

	if dir := os.Getenv("CRUSH_EDIT_EVAL_DUMP"); dir != "" {
		dumpTranscript(dir, mode, fx, msgs, got, out)
	}
	return out
}

// dumpTranscript writes the full prompt, message-by-message transcript (assistant
// text, tool calls with inputs, tool results), and the got-vs-expected diff to a
// file, for diagnosing why a task failed.
func dumpTranscript(dir, mode string, fx evalFixture, msgs []message.Message, got string, out evalResult) {
	_ = os.MkdirAll(dir, 0o755)
	var b strings.Builder
	status := "PASS"
	if !out.success {
		status = "FAIL"
	}
	fmt.Fprintf(&b, "# %s / %s — %s (file %s)\n", mode, fx.name, status, fx.fileName)
	fmt.Fprintf(&b, "\n## PROMPT\n%s\n", fx.prompt)
	fmt.Fprintf(&b, "\n## TRANSCRIPT\n")
	for _, m := range msgs {
		if txt := strings.TrimSpace(m.Content().Text); txt != "" {
			fmt.Fprintf(&b, "\n[%s] %s\n", m.Role, txt)
		}
		for _, tc := range m.ToolCalls() {
			fmt.Fprintf(&b, "\n[%s → %s] %s\n", m.Role, tc.Name, tc.Input)
		}
		for _, tr := range m.ToolResults() {
			tag := "ok"
			if tr.IsError {
				tag = "ERROR"
			}
			fmt.Fprintf(&b, "\n[result %s %s] %s\n", tr.Name, tag, tr.Content)
		}
	}
	fmt.Fprintf(&b, "\n## EXPECTED\n%s\n", fx.expected)
	fmt.Fprintf(&b, "\n## GOT\n%s\n", got)
	_ = os.WriteFile(filepath.Join(dir, mode+"__"+fx.name+".txt"), []byte(b.String()), 0o644)
}

func buildEvalPrompt(env fakeEnv, model fantasy.LanguageModel) (string, error) {
	fixedTime := func() time.Time {
		tm, _ := time.Parse("1/2/2006", "1/1/2025")
		return tm
	}
	p, err := coderPrompt(
		prompt.WithTimeFunc(fixedTime),
		prompt.WithPlatform("linux"),
		prompt.WithWorkingDir(filepath.ToSlash(env.workingDir)),
	)
	if err != nil {
		return "", err
	}
	cfg, err := config.Init(env.workingDir, "", false)
	if err != nil {
		return "", err
	}
	cfg.Config().Options.SkillsPaths = nil
	cfg.Config().Options.ContextPaths = nil
	cfg.Config().Options.GlobalContextPaths = nil
	cfg.Config().LSP = nil
	return p.Build(context.TODO(), model.Provider(), model.Model(), cfg)
}

func normalizeLF(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

type modeAgg struct {
	total, passed         int
	editCalls, editErrors int
	tokens                int64
	runErrors             int
	misfires              int64
	failed                []string
}

// evalRecord is one fixture's machine-readable outcome. Runs are compared
// across tool-surface arms by pairing these on (mode, fixture), which is what
// makes a paired significance test possible rather than eyeballing two pass
// rates.
type evalRecord struct {
	Mode     string `json:"mode"`
	Fixture  string `json:"fixture"`
	Pad      string `json:"pad"`
	Pass     bool   `json:"pass"`
	Edits    int    `json:"edits"`
	EditErrs int    `json:"edit_errors"`
	Misfires int64  `json:"misfires"`
	Tokens   int64  `json:"tokens"`
	RunErr   string `json:"run_error,omitempty"`
}

func TestEditEval(t *testing.T) {
	if os.Getenv("CRUSH_EDIT_EVAL") == "" {
		t.Skip("set CRUSH_EDIT_EVAL=1 to run the live edit-tool eval")
	}

	fixtures := loadEvalFixtures(t)
	if os.Getenv("CRUSH_EDIT_EVAL_SPREAD") != "" {
		fixtures = oncePerCategory(fixtures)
	}
	if only := os.Getenv("CRUSH_EDIT_EVAL_ONLY"); only != "" {
		want := strings.Split(only, ",")
		var kept []evalFixture
		for _, fx := range fixtures {
			for _, w := range want {
				if strings.Contains(fx.name, strings.TrimSpace(w)) {
					kept = append(kept, fx)
					break
				}
			}
		}
		fixtures = kept
	}
	if reps := envInt("CRUSH_EDIT_EVAL_REPEAT", 1); reps > 1 {
		var expanded []evalFixture
		for r := 0; r < reps; r++ {
			for _, fx := range fixtures {
				fx2 := fx
				fx2.name = fmt.Sprintf("%s#r%d", fx.name, r+1)
				expanded = append(expanded, fx2)
			}
		}
		fixtures = expanded
	}
	if limit := envInt("CRUSH_EDIT_EVAL_LIMIT", 0); limit > 0 && limit < len(fixtures) {
		fixtures = fixtures[:limit]
	}

	modes := []string{config.EditModeString, config.EditModeHashline}
	if v := os.Getenv("CRUSH_EDIT_EVAL_MODES"); v != "" {
		modes = strings.Split(v, ",")
	}

	model := haikuSubscriptionModel(t)
	aggs := map[string]*modeAgg{}
	for _, mode := range modes {
		aggs[mode] = &modeAgg{}
	}

	var misfires atomic.Int64
	pad, padBytes := mcpPadTools(t, &misfires)
	padLabel := cmp.Or(os.Getenv("CRUSH_EDIT_EVAL_MCP_PAD"), "none")
	t.Logf("tool surface: %d padding schemas (%d B) from CRUSH_EDIT_EVAL_MCP_PAD=%q",
		len(pad), padBytes, padLabel)

	// Per-fixture records, for pairing arms in analysis.
	var records []evalRecord

	// Run tasks concurrently with a bounded worker pool. Each task is fully
	// isolated (own temp dir, DB, session), so the only shared state is the
	// aggregators, guarded by mu. Concurrency is capped to stay within the
	// model provider's rate limits.
	sem := make(chan struct{}, envInt("CRUSH_EDIT_EVAL_CONCURRENCY", 4))
	var mu sync.Mutex

	t.Run("runs", func(t *testing.T) {
		for _, mode := range modes {
			for _, fx := range fixtures {
				mode, fx := mode, fx
				t.Run(mode+"/"+fx.name, func(t *testing.T) {
					t.Parallel()
					sem <- struct{}{}
					defer func() { <-sem }()

					r := runEditTask(t, model, mode, fx, pad, &misfires)

					mu.Lock()
					defer mu.Unlock()
					agg := aggs[mode]
					agg.total++
					agg.editCalls += r.editCalls
					agg.editErrors += r.editErrors
					agg.tokens += r.tokens
					agg.misfires += r.misfires
					rec := evalRecord{
						Mode: mode, Fixture: fx.name, Pad: padLabel, Pass: r.success,
						Edits: r.editCalls, EditErrs: r.editErrors,
						Misfires: r.misfires, Tokens: r.tokens,
					}
					if r.runErr != nil {
						rec.RunErr = r.runErr.Error()
					}
					records = append(records, rec)
					if r.misfires > 0 {
						t.Logf("[%s] %s: %d call(s) into the inert MCP surface", mode, fx.name, r.misfires)
					}
					if r.runErr != nil {
						agg.runErrors++
						t.Logf("[%s] %s: run error: %v", mode, fx.name, r.runErr)
					}
					if r.success {
						agg.passed++
					} else {
						agg.failed = append(agg.failed, fx.name)
						t.Logf("[%s] %s FAILED (edits=%d errs=%d)", mode, fx.name, r.editCalls, r.editErrors)
						for i, in := range r.editInputs {
							t.Logf("  edit-call[%d] input: %s", i, truncate(in, 400))
						}
						for i, e := range r.errSamples {
							t.Logf("  edit-err[%d]: %s", i, truncate(e, 300))
						}
					}
				})
			}
		}
	})

	// The "runs" subtest returns only after all its parallel children finish,
	// so the aggregates are complete here.
	printScoreboard(t, evalModelID, len(fixtures), modes, aggs)
	writeEvalRecords(t, records)
}

// writeEvalRecords appends per-fixture outcomes as JSONL when
// CRUSH_EDIT_EVAL_RESULTS is set, so two arms can be paired for analysis.
func writeEvalRecords(t *testing.T, records []evalRecord) {
	path := os.Getenv("CRUSH_EDIT_EVAL_RESULTS")
	if path == "" || len(records) == 0 {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Logf("could not write eval records: %v", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			t.Logf("could not encode eval record: %v", err)
			return
		}
	}
	t.Logf("wrote %d eval records to %s", len(records), path)
}

func printScoreboard(t *testing.T, modelID string, n int, modes []string, aggs map[string]*modeAgg) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n=== Edit-tool eval — %s — %d fixtures ===\n", modelID, n)
	fmt.Fprintf(&b, "%-10s  %8s  %10s  %11s  %9s  %8s  %9s\n", "mode", "pass", "pass%", "avg edits", "edit err", "avg tok", "misfires")
	for _, mode := range modes {
		a := aggs[mode]
		if a.total == 0 {
			continue
		}
		fmt.Fprintf(
			&b, "%-10s  %4d/%-3d  %9.1f%%  %11.2f  %9.2f  %8d  %9d\n",
			mode, a.passed, a.total,
			100*float64(a.passed)/float64(a.total),
			float64(a.editCalls)/float64(a.total),
			float64(a.editErrors)/float64(a.total),
			a.tokens/int64(a.total),
			a.misfires,
		)
	}
	for _, mode := range modes {
		if a := aggs[mode]; len(a.failed) > 0 {
			sort.Strings(a.failed)
			fmt.Fprintf(&b, "%s failed: %s\n", mode, strings.Join(a.failed, ", "))
		}
	}
	t.Log(b.String())
}

// oncePerCategory keeps the first fixture of each category ("<category>-NNN"),
// giving a representative spread across all 20 mutation kinds.
func oncePerCategory(fixtures []evalFixture) []evalFixture {
	seen := map[string]bool{}
	var out []evalFixture
	for _, fx := range fixtures {
		cat := fx.name
		if i := strings.LastIndex(fx.name, "-"); i >= 0 {
			cat = fx.name[:i]
		}
		if seen[cat] {
			continue
		}
		seen[cat] = true
		out = append(out, fx)
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

var _ = message.Assistant
