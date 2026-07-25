package agent

// Benefit-side evaluation for the MCP tool surface.
//
// The tool-surface A/B (see eval_toolpad_test.go) measured only the *cost* of
// carrying 98 MCP schemas: on single-file edit tasks the surface is dead weight,
// and it showed no accuracy effect and zero misfires. That is half the trade.
// The other half is what the surface buys on tasks that genuinely need an MCP
// tool, where eagerly-loaded schemas save a discovery round-trip.
//
// Three arms, run against the same tasks:
//
//	eager     all 98 real schemas in context; the target tools work.
//	deferred  the same 98 tools listed by name with a one-line stub and no
//	          parameters — roughly what Claude Code sends for deferred tools —
//	          plus a tool_search tool that returns a tool's real schema on
//	          demand. Costs a round-trip instead of prefix tokens.
//	absent    target tool not present at all. This is a validity control: if a
//	          task passes here, the task did not actually require the tool and
//	          the fixture is broken.
//
// Tasks are scored deterministically. Each canned tool response carries a magic
// token that appears nowhere else, so the answer cannot be guessed or recalled
// from pretraining — the model can only produce it by calling the tool and
// reading the result.
//
// Tool selection and weighting come from real recorded usage across every
// project database on this machine (grafana query_prometheus dominates at 113
// calls, context7 next), so the benchmark reflects how these servers are
// actually used rather than a hypothetical.
//
// Env knobs:
//
//	CRUSH_MCP_BENEFIT_EVAL=1      enable (required; makes real model calls)
//	CRUSH_MCP_BENEFIT_ARMS=…      comma list (default "eager,deferred,absent")
//	CRUSH_MCP_BENEFIT_MODEL=…     model id (default: the edit eval's model)
//	CRUSH_MCP_BENEFIT_RESULTS=…   append per-task JSONL here
//	CRUSH_MCP_BENEFIT_CONCURRENCY=N

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// benefitTask is one MCP-requiring task with a deterministic answer.
type benefitTask struct {
	name string
	// prompt is phrased as a user would: it names what is wanted, not which
	// tool to call. Picking the tool is part of what is being measured.
	prompt string
	// wantTool is the tool that can answer it.
	wantTool string
	// canned is the JSON the tool returns, containing wantAnswer.
	canned string
	// wantAnswer is the magic token the final response must contain.
	wantAnswer string
}

// benefitTasks are weighted toward the tools with real recorded usage.
var benefitTasks = []benefitTask{
	{
		name:       "prom-p99-latency",
		prompt:     "What is the current p99 checkout latency in production? The prod Prometheus datasource uid is prod-metrics. Answer with the number of milliseconds.",
		wantTool:   "mcp__grafana__query_prometheus",
		canned:     `{"resultType":"vector","result":[{"metric":{"quantile":"0.99","service":"checkout"},"value":[1784950000,"473"]}]}`,
		wantAnswer: "473",
	},
	{
		name:       "prom-error-rate",
		prompt:     "Report the current 5xx error rate for the api service from Prometheus (datasource uid prod-metrics). Give the exact value you find.",
		wantTool:   "mcp__grafana__query_prometheus",
		canned:     `{"resultType":"vector","result":[{"metric":{"service":"api"},"value":[1784950000,"0.0271"]}]}`,
		wantAnswer: "0.0271",
	},
	{
		name:       "prom-pod-restarts",
		prompt:     "How many times has the payments pod restarted in the last hour? Use the prod-metrics Prometheus datasource and report the count.",
		wantTool:   "mcp__grafana__query_prometheus",
		canned:     `{"resultType":"vector","result":[{"metric":{"pod":"payments-7d9"},"value":[1784950000,"17"]}]}`,
		wantAnswer: "17",
	},
	{
		name:       "prom-queue-depth",
		prompt:     "What is the current depth of the ingest queue in production? Prometheus datasource uid is prod-metrics. Report the number.",
		wantTool:   "mcp__grafana__query_prometheus",
		canned:     `{"resultType":"vector","result":[{"metric":{"queue":"ingest"},"value":[1784950000,"8241"]}]}`,
		wantAnswer: "8241",
	},
	{
		name:       "loki-oom-message",
		prompt:     "Find the most recent OOM log line for the worker service in production logs (Loki datasource uid prod-logs) and quote the reported limit exactly.",
		wantTool:   "mcp__grafana__query_loki_logs",
		canned:     `{"streams":[{"labels":{"service":"worker"},"entries":[{"ts":"2026-07-24T10:00:00Z","line":"OOMKilled: exceeded memory limit of 3172Mi"}]}]}`,
		wantAnswer: "3172Mi",
	},
	{
		name:       "loki-deploy-id",
		prompt:     "Which deploy id is mentioned in the latest rollout log line for the gateway service? Loki datasource uid is prod-logs.",
		wantTool:   "mcp__grafana__query_loki_logs",
		canned:     `{"streams":[{"labels":{"service":"gateway"},"entries":[{"ts":"2026-07-24T09:12:00Z","line":"rollout complete deploy_id=dpl-9f4c21"}]}]}`,
		wantAnswer: "dpl-9f4c21",
	},
	{
		name:       "alert-rule-threshold",
		prompt:     "What threshold is configured on the alert rule that watches disk pressure? Report the exact threshold value.",
		wantTool:   "mcp__grafana__list_alert_rules",
		canned:     `{"rules":[{"title":"DiskPressureHigh","condition":"disk_used_percent > 91.5","uid":"ar-disk-1"}]}`,
		wantAnswer: "91.5",
	},
	{
		name:       "alert-rule-name",
		prompt:     "Which alert rule currently covers certificate expiry? Give its exact title.",
		wantTool:   "mcp__grafana__list_alert_rules",
		canned:     `{"rules":[{"title":"CertExpiryWarn7d","condition":"cert_days_left < 7","uid":"ar-cert-9"}]}`,
		wantAnswer: "CertExpiryWarn7d",
	},
	{
		name:       "datasource-uid",
		prompt:     "What is the uid of the Prometheus datasource configured in Grafana? Report it exactly.",
		wantTool:   "mcp__grafana__list_datasources",
		canned:     `[{"name":"Prod Metrics","type":"prometheus","uid":"ds-prom-x71b"}]`,
		wantAnswer: "ds-prom-x71b",
	},
	{
		name:       "incident-id",
		prompt:     "Is there an open incident right now? If so report its id exactly.",
		wantTool:   "mcp__grafana__list_incidents",
		canned:     `{"incidents":[{"id":"inc-4482-zz","status":"active","title":"Elevated checkout errors"}]}`,
		wantAnswer: "inc-4482-zz",
	},
	{
		name:       "docs-config-key",
		prompt:     "In the zod library docs (library id /colinhacks/zod), what is the exact config key that enables strict parsing? Report the key.",
		wantTool:   "mcp__context7__query-docs",
		canned:     `{"snippets":[{"title":"Strict parsing","content":"Set the config key zodStrictParseV7 to true to reject unknown keys."}]}`,
		wantAnswer: "zodStrictParseV7",
	},
	{
		name:       "docs-default-timeout",
		prompt:     "According to the docs for library id /vercel/next.js, what is the default value of the fetch revalidate window? Report the exact value.",
		wantTool:   "mcp__context7__query-docs",
		canned:     `{"snippets":[{"title":"Revalidation","content":"The default revalidate window is 3607 seconds unless overridden."}]}`,
		wantAnswer: "3607",
	},
	{
		name:       "resolve-library",
		prompt:     "Find the canonical library id used by the docs service for the package named 'drizzle-orm' and report it exactly.",
		wantTool:   "mcp__context7__resolve-library-id",
		canned:     `{"matches":[{"libraryName":"drizzle-orm","libraryId":"/drizzle-team/drizzle-orm-v42"}]}`,
		wantAnswer: "/drizzle-team/drizzle-orm-v42",
	},
	{
		name:       "memory-recall",
		prompt:     "What did I previously record about our database migration policy? Report the recorded detail exactly.",
		wantTool:   "mcp__mem0__recall",
		canned:     `{"memories":[{"text":"Migrations must run behind flag MIGRATE_GATE_88 before promotion."}]}`,
		wantAnswer: "MIGRATE_GATE_88",
	},
	{
		name:       "board-column",
		prompt:     "What is on the team board right now? Report the exact name of the column that holds blocked work.",
		wantTool:   "mcp__mom__mom_board",
		canned:     `{"columns":[{"name":"Blocked-Waiting-Ext","cards":3},{"name":"Doing","cards":5}]}`,
		wantAnswer: "Blocked-Waiting-Ext",
	},
}

// cannedTool serves a fixed payload and records that it was called.
type cannedTool struct {
	info   fantasy.ToolInfo
	body   string
	called *atomic.Int64
	args   *sync.Map // call index -> raw input, for well-formedness checks
	opts   fantasy.ProviderOptions
}

func (c *cannedTool) Info() fantasy.ToolInfo { return c.info }

func (c *cannedTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	n := c.called.Add(1)
	c.args.Store(n, call.Input)
	return fantasy.NewTextResponse(c.body), nil
}

func (c *cannedTool) ProviderOptions() fantasy.ProviderOptions     { return c.opts }
func (c *cannedTool) SetProviderOptions(o fantasy.ProviderOptions) { c.opts = o }

// searchTool is the deferred-loading stand-in: it returns the full schema for a
// named tool, which is what an eager surface would already have supplied.
type searchTool struct {
	surface map[string]mcpSchema
	calls   *atomic.Int64
	opts    fantasy.ProviderOptions
}

func (s *searchTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: "tool_search",
		Description: "Load the full documentation and parameter schema for a deferred tool. " +
			"Deferred tools are listed by name only; call this with a tool name before using it.",
		Parameters: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Exact name of the tool to load, e.g. mcp__grafana__query_prometheus",
			},
		},
		Required: []string{"name"},
	}
}

func (s *searchTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	s.calls.Add(1)
	var in struct{ Name string }
	if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
		return fantasy.NewTextErrorResponse("invalid parameters: expected {\"name\": \"<tool>\"}"), nil
	}
	sc, ok := s.surface[in.Name]
	if !ok {
		var names []string
		for n := range s.surface {
			names = append(names, n)
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"unknown tool %q; %d tools are available", in.Name, len(names))), nil
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return fantasy.NewTextErrorResponse("could not encode schema"), nil
	}
	return fantasy.NewTextResponse(string(b)), nil
}

func (s *searchTool) ProviderOptions() fantasy.ProviderOptions     { return s.opts }
func (s *searchTool) SetProviderOptions(o fantasy.ProviderOptions) { s.opts = o }

type benefitArmTools struct {
	tools       []fantasy.AgentTool
	targetCalls *atomic.Int64
	searchCalls *atomic.Int64
	misfires    *atomic.Int64
	args        *sync.Map
	promptBytes int
}

// buildBenefitTools assembles one arm's tool set for a given task.
func buildBenefitTools(t *testing.T, arm string, task benefitTask) benefitArmTools {
	t.Helper()
	surface := loadMCPSurface(t)
	byName := make(map[string]mcpSchema, len(surface))
	for _, s := range surface {
		byName[s.Name] = s
	}
	require.Contains(t, byName, task.wantTool, "task targets a tool absent from the captured surface")

	out := benefitArmTools{
		targetCalls: &atomic.Int64{},
		searchCalls: &atomic.Int64{},
		misfires:    &atomic.Int64{},
		args:        &sync.Map{},
	}

	mkCanned := func(s mcpSchema, params map[string]any, required []string) *cannedTool {
		return &cannedTool{
			info: fantasy.ToolInfo{
				Name: s.Name, Description: s.Description,
				Parameters: params, Required: required,
			},
			body: task.canned, called: out.targetCalls, args: out.args,
		}
	}
	props := func(s mcpSchema) (map[string]any, []string) {
		p, _ := s.InputSchema["properties"].(map[string]any)
		var req []string
		if raw, ok := s.InputSchema["required"].([]any); ok {
			for _, r := range raw {
				if str, ok := r.(string); ok {
					req = append(req, str)
				}
			}
		}
		return p, req
	}

	switch arm {
	case "absent":
		// Validity control: no MCP tools at all.
		return out

	case "eager":
		for _, s := range surface {
			p, req := props(s)
			if s.Name == task.wantTool {
				out.tools = append(out.tools, mkCanned(s, p, req))
			} else {
				out.tools = append(out.tools, &padTool{
					info:     fantasy.ToolInfo{Name: s.Name, Description: s.Description, Parameters: p, Required: req},
					misfires: out.misfires,
				})
			}
			if b, err := json.Marshal(s); err == nil {
				out.promptBytes += len(b)
			}
		}

	case "deferred":
		// Names plus a one-line stub and no parameters, which is roughly what a
		// deferred-tool listing costs. Real docs come from tool_search.
		out.tools = append(out.tools, &searchTool{surface: byName, calls: out.searchCalls})
		for _, s := range surface {
			stub := fantasy.ToolInfo{
				Name:        s.Name,
				Description: "Deferred tool. Call tool_search with this name to load its parameters before use.",
				Parameters:  map[string]any{},
			}
			if s.Name == task.wantTool {
				c := mkCanned(s, map[string]any{}, nil)
				c.info = stub
				out.tools = append(out.tools, c)
			} else {
				out.tools = append(out.tools, &padTool{info: stub, misfires: out.misfires})
			}
			if b, err := json.Marshal(stub); err == nil {
				out.promptBytes += len(b)
			}
		}

	default:
		t.Fatalf("unknown arm %q", arm)
	}
	return out
}

type benefitRecord struct {
	Arm         string `json:"arm"`
	Task        string `json:"task"`
	Pass        bool   `json:"pass"`
	CalledTool  bool   `json:"called_target_tool"`
	SearchCalls int64  `json:"search_calls"`
	Misfires    int64  `json:"misfires"`
	Assistant   int    `json:"assistant_turns"`
	ToolCalls   int    `json:"tool_calls"`
	InTokens    int64  `json:"input_tokens"`
	OutTokens   int64  `json:"output_tokens"`
	CacheRead   int64  `json:"cache_read"`
	CacheWrite  int64  `json:"cache_write"`
	Units       int64  `json:"cost_units"`
	PromptBytes int    `json:"tool_schema_bytes"`
	RunErr      string `json:"run_error,omitempty"`
}

func TestMCPBenefitEval(t *testing.T) {
	if os.Getenv("CRUSH_MCP_BENEFIT_EVAL") == "" {
		t.Skip("set CRUSH_MCP_BENEFIT_EVAL=1 to run the live MCP benefit eval")
	}

	arms := strings.Split(envOr("CRUSH_MCP_BENEFIT_ARMS", "eager,deferred,absent"), ",")
	model := haikuSubscriptionModel(t)

	sem := make(chan struct{}, envInt("CRUSH_MCP_BENEFIT_CONCURRENCY", 4))
	var mu sync.Mutex
	var records []benefitRecord

	t.Run("runs", func(t *testing.T) {
		for _, arm := range arms {
			for _, task := range benefitTasks {
				arm, task := strings.TrimSpace(arm), task
				t.Run(arm+"/"+task.name, func(t *testing.T) {
					t.Parallel()
					sem <- struct{}{}
					defer func() { <-sem }()

					rec := runBenefitTask(t, model, arm, task)
					mu.Lock()
					records = append(records, rec)
					mu.Unlock()

					if !rec.Pass {
						t.Logf("[%s] %s: no answer (tool called=%v, searches=%d, turns=%d)",
							arm, task.name, rec.CalledTool, rec.SearchCalls, rec.Assistant)
					}
				})
			}
		}
	})

	printBenefitScoreboard(t, arms, records)
	writeBenefitRecords(t, records)
}

func runBenefitTask(t *testing.T, model fantasy.LanguageModel, arm string, task benefitTask) benefitRecord {
	env := testEnv(t)
	at := buildBenefitTools(t, arm, task)

	sysPrompt, err := buildEvalPrompt(env, model)
	require.NoError(t, err)

	agent := testSessionAgent(env, model, model, sysPrompt, at.tools...)
	session, err := env.sessions.Create(t.Context(), "mcp-benefit")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	res, runErr := agent.Run(ctx, SessionAgentCall{
		Prompt:          task.prompt,
		SessionID:       session.ID,
		MaxOutputTokens: 4000,
		NonInteractive:  true,
	})

	rec := benefitRecord{
		Arm: arm, Task: task.name,
		SearchCalls: at.searchCalls.Load(),
		Misfires:    at.misfires.Load(),
		CalledTool:  at.targetCalls.Load() > 0,
		PromptBytes: at.promptBytes,
	}
	if runErr != nil {
		rec.RunErr = runErr.Error()
	}
	if res != nil {
		u := res.TotalUsage
		rec.InTokens, rec.OutTokens = u.InputTokens, u.OutputTokens
		rec.CacheRead, rec.CacheWrite = u.CacheReadTokens, u.CacheCreationTokens
		// Cost-equivalent input units: writes bill 1.25x base input, reads 0.1x.
		rec.Units = u.InputTokens + int64(float64(u.CacheCreationTokens)*1.25) +
			int64(float64(u.CacheReadTokens)*0.1)
	}

	// Score on the final assistant text: the magic token can only come from the
	// tool result, so containing it means the tool was actually used to answer.
	msgs, _ := env.messages.List(t.Context(), session.ID)
	var lastText string
	for _, m := range msgs {
		if m.Role == "assistant" {
			rec.Assistant++
			rec.ToolCalls += len(m.ToolCalls())
			if txt := strings.TrimSpace(m.Content().Text); txt != "" {
				lastText = txt
			}
		}
	}
	rec.Pass = strings.Contains(strings.ToLower(lastText), strings.ToLower(task.wantAnswer))
	return rec
}

func printBenefitScoreboard(t *testing.T, arms []string, records []benefitRecord) {
	byArm := map[string][]benefitRecord{}
	for _, r := range records {
		byArm[r.Arm] = append(byArm[r.Arm], r)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n=== MCP benefit eval — %d tasks ===\n", len(benefitTasks))
	fmt.Fprintf(&b, "%-9s %9s %9s %8s %8s %8s %9s %10s %9s\n",
		"arm", "pass", "used tool", "searches", "turns", "calls", "misfires", "units/task", "schema B")
	for _, arm := range arms {
		arm = strings.TrimSpace(arm)
		rs := byArm[arm]
		if len(rs) == 0 {
			continue
		}
		var pass, used int
		var searches, misfires, units int64
		var turns, calls, bytes int
		for _, r := range rs {
			if r.Pass {
				pass++
			}
			if r.CalledTool {
				used++
			}
			searches += r.SearchCalls
			misfires += r.Misfires
			units += r.Units
			turns += r.Assistant
			calls += r.ToolCalls
			bytes = r.PromptBytes
		}
		n := len(rs)
		fmt.Fprintf(&b, "%-9s %4d/%-4d %9d %8.2f %8.2f %8.2f %9d %10d %9d\n",
			arm, pass, n, used, float64(searches)/float64(n), float64(turns)/float64(n),
			float64(calls)/float64(n), misfires, units/int64(n), bytes)
	}
	t.Log(b.String())
}

func writeBenefitRecords(t *testing.T, records []benefitRecord) {
	path := os.Getenv("CRUSH_MCP_BENEFIT_RESULTS")
	if path == "" || len(records) == 0 {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Logf("could not write benefit records: %v", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			t.Logf("could not encode benefit record: %v", err)
			return
		}
	}
	t.Logf("wrote %d benefit records to %s", len(records), path)
}
