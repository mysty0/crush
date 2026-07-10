package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// testDriverMsg lets the test inject arbitrary code onto the running
// [tea.Program]'s own event-loop goroutine via [tea.WithFilter], which
// bubbletea guarantees runs synchronously inline in eventLoop before
// Update is invoked (see tea.go's eventLoop: "msg = p.filter(model,
// msg)" runs on the same goroutine that later calls Update/View). This
// is the only race-free way to call UI methods directly against a
// *running* Program without either modifying production Update() to
// recognize a test-only message type, or faking real key/mouse input
// to reach deeply-nested navigation state (e.g. the agent picker's
// exact focus/selection preconditions) that is irrelevant to the bug
// under test. fn's return value, if non-nil, is discarded — this
// harness does not need to chain further Cmds from injected calls.
type testDriverMsg struct {
	fn func(*UI)
}

// driverFilter implements tea.WithFilter: it intercepts testDriverMsg,
// runs its fn synchronously (safe: same goroutine as Update/View), and
// swallows the message so Update's switch (which has no case for it)
// never sees it. Every other message passes through unchanged.
func driverFilter(model tea.Model, msg tea.Msg) tea.Msg {
	if d, ok := msg.(testDriverMsg); ok {
		d.fn(model.(*UI))
		return nil
	}
	return msg
}

// syncDo sends fn to the running program and blocks until it has
// actually executed on the event-loop goroutine (not just been
// enqueued — Program.Send's unbuffered channel only guarantees the
// message was *received*, not that the filter has finished running).
// Use this for deterministic, race-free setup/inspection against a
// live UI; anything after syncDo returns is guaranteed to see fn's
// effects.
func syncDo(t *testing.T, p *tea.Program, fn func(*UI)) {
	t.Helper()
	done := make(chan struct{})
	p.Send(testDriverMsg{fn: func(u *UI) {
		fn(u)
		close(done)
	}})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("test driver message did not execute — event loop appears stuck")
	}
}

// stubWorkspace wraps a real [workspace.Workspace] (backed by the real
// app/session/message services under test) and only overrides Config,
// which the embedded workspace.AppWorkspace would otherwise resolve
// through a full on-disk [config.ConfigStore] we don't need for this
// reproduction.
type stubWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

func (s *stubWorkspace) Config() *config.Config { return s.cfg }

// TestE2E_SubAgentBashFreeze is the end-to-end regression guard for
// a TUI freeze a user hit live: spawn a sub-agent, wait, open its
// fullscreen live view, scroll its history, then have the sub-agent
// start running a bash tool. Before animation scheduling was
// centralized into the UI's single clock (see UI.startAnimClock),
// the same tool call was animated by two independent per-item tick
// chains — one for the nested item inside the main chat's
// AgentToolMessageItem, one for the same tool-call ID in the
// fullscreen sub-agent chat — and each animation frame handled by
// both chains scheduled two follow-up frames. That gain-two feedback
// loop doubled the number of concurrent tick chains every frame; the
// live process was recovered at 1.36M goroutines, 6.4GB RSS, with
// keyboard input starved behind ~1M queued animation messages
// (confirmed via delve and pprof; this test codifies that exact user
// flow).
//
// Every piece of machinery implicated by that investigation is real,
// not simulated: a real SQLite-backed session.Service and
// message.Service (so message debounce/flush timing matches
// production), a real app.App wired via [app.NewForTestWithSessions]
// (so pubsub fan-out — session/message/bash-progress events all
// funneling into one broker read by [app.App.Subscribe] — matches
// production exactly), and a real, running [tea.Program] driving the
// actual UI (so bubbletea's own per-Cmd goroutine dispatch —
// execBatchMsg/handleCommands, which has zero backpressure — is
// exercised, not bypassed). Only the LLM/agent coordinator is absent:
// this test plays the sub-agent's role itself, publishing the same
// message and bash-progress events a real running tool call would.
//
// Safety: the burst phase is bounded on two independent axes so a
// regressed run cannot consume unbounded CI resources — a wall-clock
// ceiling and a goroutine-count ceiling, whichever comes first.
func TestE2E_SubAgentBashFreeze(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real DB, app, and tea.Program; skipped in -short")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// --- Real persistence: SQLite-backed sessions and messages, wired
	// with the exact production debounce behavior. ---
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	// --- Real app wiring: session/message/bash-progress events fan
	// into one broker exactly as production's setupEvents does. ---
	a := app.NewForTestWithSessions(ctx, sessions, messages)
	t.Cleanup(a.ShutdownForTest)
	// A minimal, real skills.Manager: AppWorkspace.ListSkills (invoked
	// from Init's loadCustomCommands) dereferences app.Skills
	// unconditionally, unlike the AgentCoordinator/LSPManager fields
	// this test leaves nil.
	a.Skills = skills.NewManager(nil, nil, nil)

	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Agents:    map[string]config.Agent{},
		Options: &config.Options{
			TUI: &config.TUIOptions{},
			// Avoid a real network call to Catwalk/Hyper for the
			// provider list, which Init's Cmds trigger unconditionally.
			DisableProviderAutoUpdate: true,
		},
	}
	ws := &stubWorkspace{Workspace: workspace.NewAppWorkspace(a, config.NewTestStore(cfg)), cfg: cfg}
	com := common.DefaultCommon(ws)

	ui := New(com, "", false)

	// --- Spawn sub-agent: a parent session with an in-progress "agent"
	// tool call, and the sub-agent's own session, exactly as
	// coordinator.runSubAgent creates them in production. ---
	parentSession, err := sessions.Create(ctx, "main session")
	require.NoError(t, err)

	// The prompt is deliberately long (~9KB): the live frozen process
	// was caught inside drawSubAgentBanner truncating a 9329-character
	// prompt string on every single Draw call (confirmed via delve —
	// frame inspection showed len(s) == 9329 at ansi.truncate), so a
	// short test prompt would understate a real, unconditional
	// per-frame cost this reproduction depends on.
	const agentCallID = "toolu_agent_1"
	longPrompt := "Investigate NVIDIA driver unaccounted VRAM usage in a Vulkan app. " +
		strings.Repeat("Already ruled out: this specific allocation path, verified via RenderDoc resource enumeration and gpu memory tracking; continuing to the next candidate. ", 110)
	agentInput, err := json.Marshal(agent.AgentParams{Prompt: longPrompt})
	require.NoError(t, err)
	assistantMsg, err := messages.Create(ctx, parentSession.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: agentCallID, Name: agent.AgentToolName, Input: string(agentInput), Finished: false},
		},
	})
	require.NoError(t, err)

	subSessionID := sessions.CreateAgentToolSessionID(assistantMsg.ID, agentCallID)
	subSession, err := sessions.CreateTaskSession(ctx, subSessionID, parentSession.ID, "VRAM investigation")
	require.NoError(t, err)

	// Substantial pre-existing history in the sub-agent session —
	// matching the live frozen process's session (648 messages,
	// confirmed via delve), a fresh handful-of-messages session
	// understates the real per-render cost enough that no backlog
	// forms at all (confirmed: an earlier version of this test with a
	// ~10-message history and short prompt showed goroutine growth of
	// only +4, i.e. no reproduction) — real-world session size turns
	// out to be load-bearing for the freeze, not incidental.
	const priorHistoryMessages = 220
	for i := range priorHistoryMessages {
		_, err := messages.Create(ctx, subSession.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: fmt.Sprintf(
					"Checked candidate cause #%d, ruled out after reviewing the disassembly "+
						"and cross-referencing against the driver's allocation call sites. "+
						"%s", i, strings.Repeat("Detail. ", 20),
				)},
				message.ToolCall{
					ID:       fmt.Sprintf("toolu_hist_%d", i),
					Name:     "fetch",
					Input:    `{"url":"https://example.com/docs/nvidia-driver-internals"}`,
					Finished: true,
				},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
		require.NoError(t, err)
		_, err = messages.Create(ctx, subSession.ID, message.CreateMessageParams{
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{
					ToolCallID: fmt.Sprintf("toolu_hist_%d", i),
					Content:    strings.Repeat("fetched documentation line\n", 15),
				},
			},
		})
		require.NoError(t, err)
	}

	// --- Run the real UI through a real, running tea.Program. This is
	// the exact production entry point (internal/cmd/root.go) besides
	// the headless output/input options. ---
	p := tea.NewProgram(
		ui,
		tea.WithContext(ctx),
		tea.WithInput(nil),
		tea.WithOutput(io.Discard),
		tea.WithoutSignals(),
		tea.WithoutSignalHandler(),
		tea.WithFilter(driverFilter),
	)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = p.Run()
	}()
	t.Cleanup(func() {
		p.Kill()
		<-runDone
	})

	go a.Subscribe(p)

	p.Send(tea.WindowSizeMsg{Width: 120, Height: 42})

	// Seed the UI directly into the chat state a real session load
	// would produce, bypassing the async session-switch flow (dialog
	// selection, DB round trip) that is irrelevant to this
	// reproduction — see syncDo's doc comment for why this is safe.
	syncDo(t, p, func(u *UI) {
		u.state = uiChat
		u.session = &parentSession
		u.setSessionMessages([]message.Message{assistantMsg})
	})

	// "waited a bit"
	time.Sleep(300 * time.Millisecond)

	// "went to subagent": the real production method, invoked directly
	// (see testDriverMsg's doc comment for why not via simulated
	// keypresses through the agent-list picker).
	syncDo(t, p, func(u *UI) {
		u.enterSubAgentView(subSession.ID, agentCallID, "Investigate VRAM usage")
		hist, err := messages.List(ctx, subSession.ID)
		require.NoError(t, err)
		items := u.buildSubAgentMessageItems(hist)
		u.subAgentChat.SetMessages(items...)
		u.subAgentChat.SelectLast()
		u.subAgentChat.ScrollToBottom()
	})

	// "scrolled the history"
	syncDo(t, p, func(u *UI) {
		for range 5 {
			u.subAgentChat.ScrollBy(-3)
		}
	})

	baseline := runtime.NumGoroutine()
	t.Logf("baseline goroutines after setup: %d", baseline)

	// --- "subagent starts doing bash": the actual trigger. Real
	// message.Service.Update calls (streaming assistant text, real
	// ~33ms debounce) and real agenttools.PublishBashProgress calls
	// (real 100ms cadence, full-accumulated-output-per-tick — the
	// exact production shape) running concurrently, exactly as a live
	// bash tool call and its enclosing turn would produce. ---
	burstCtx, stopBurst := context.WithCancel(ctx)
	defer stopBurst()

	burstDone := make(chan struct{}, 2)

	// Streaming assistant text on the sub-agent's own turn.
	go func() {
		defer func() { burstDone <- struct{}{} }()
		streamMsg, err := messages.Create(ctx, subSession.ID, message.CreateMessageParams{
			Role: message.Assistant,
		})
		if err != nil {
			return
		}
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		text := ""
		for {
			select {
			case <-burstCtx.Done():
				return
			case <-ticker.C:
				text += "token "
				streamMsg.AppendContent("token ")
				if err := messages.Update(ctx, streamMsg); err != nil {
					return
				}
			}
		}
	}()

	// A sequence of bash tool calls, each streaming progress at the
	// real 100ms cadence with growing accumulated output — the
	// mechanism confirmed (via CPU profile + live memory inspection)
	// to dominate cost per event, run back-to-back so several
	// Animatable items are alive/spinning at once, matching the
	// duplicate-ticker mechanism found live (one Anim in the mirrored
	// nested item inside the main chat's AgentToolMessageItem, a
	// second, independent Anim for the same tool-call ID in the
	// fullscreen sub-agent chat).
	go func() {
		defer func() { burstDone <- struct{}{} }()
		for call := range 4 {
			select {
			case <-burstCtx.Done():
				return
			default:
			}
			callID := fmt.Sprintf("toolu_bash_%d", call)
			input, _ := json.Marshal(agenttools.BashParams{
				Description: "run diagnostic",
				Command:     "objdump -d /nix/store/.../libnvidia-gpucomp.so | grep -c malloc",
			})
			bashMsg, err := messages.Create(ctx, subSession.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.ToolCall{ID: callID, Name: agenttools.BashToolName, Input: string(input), Finished: false},
				},
			})
			if err != nil {
				return
			}
			ticker := time.NewTicker(100 * time.Millisecond)
			var output strings.Builder
			// A ~64KB chunk of realistic disassembly-line text, appended
			// whole each tick. This matches the actual command recovered
			// from the live incident's sibling process
			// (`objdump -d libnvidia-gpucomp.so | grep -c ...`): objdump
			// -d on a several-hundred-MB shared library streams a large
			// volume of output very quickly, not the few bytes a short
			// synthetic command would produce. PublishBashProgress
			// (bash_progress.go) sends the FULL accumulated buffer on
			// every tick, and the render path (toolOutputPlainContent ->
			// RemapANSI16/NormalizeSpace, confirmed dominant in the live
			// CPU profile) reprocesses that whole buffer from scratch
			// each time — an O(buffer size) cost paid every 100ms as the
			// buffer itself grows without bound, i.e. cumulative cost
			// grows faster than linearly over the run. A short, ~1KB
			// synthetic buffer (this test's first version) understates
			// that cost enough that no backlog ever formed.
			chunk := strings.Repeat("  4015a3:\t48 89 e5\tmov %rsp,%rbp ; objdump -d disasm line\n", 900)
			for range 25 {
				select {
				case <-burstCtx.Done():
					ticker.Stop()
					return
				case <-ticker.C:
					output.WriteString(chunk)
					agenttools.PublishBashProgress(subSession.ID, callID, output.String())
				}
			}
			ticker.Stop()

			tc := bashMsg.ToolCalls()[0]
			tc.Finished = true
			bashMsg.Parts = []message.ContentPart{tc, message.Finish{Reason: message.FinishReasonEndTurn}}
			if err := messages.Update(ctx, bashMsg); err != nil {
				return
			}
			if _, err := messages.Create(ctx, subSession.ID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{ToolCallID: callID, Content: fmt.Sprintf("%d bytes of disassembly output", output.Len())},
				},
			}); err != nil {
				return
			}
		}
	}()

	// Growth monitor: bounded on wall-clock time AND goroutine growth,
	// whichever comes first. On a healthy run the burst simply runs to
	// its wall-clock end; a regression to multiplying animation chains
	// blows through the ceiling within a couple of seconds and stops
	// the burst early, keeping a failing run cheap.
	const (
		maxBurstDuration = 20 * time.Second
		goroutineCeiling = 20000
	)
	deadline := time.Now().Add(maxBurstDuration)
	peak := baseline
	sampleTicker := time.NewTicker(100 * time.Millisecond)
	defer sampleTicker.Stop()
	sample := 0
monitor:
	for {
		select {
		case <-sampleTicker.C:
			n := runtime.NumGoroutine()
			if n > peak {
				peak = n
			}
			sample++
			if sample%5 == 0 { // log roughly every 500ms
				t.Logf("t=%s goroutines=%d", time.Since(deadline.Add(-maxBurstDuration)).Round(100*time.Millisecond), n)
			}
			if n >= baseline+goroutineCeiling || time.Now().After(deadline) {
				break monitor
			}
		case <-ctx.Done():
			break monitor
		}
	}
	stopBurst()

	// Drain the burst goroutines (bounded by burstCtx already being
	// canceled above).
	for range 2 {
		select {
		case <-burstDone:
		case <-time.After(5 * time.Second):
			t.Fatal("burst goroutine did not exit after cancellation")
		}
	}

	dbMsgs, _ := messages.List(ctx, subSession.ID)
	t.Logf("diagnostic: %d messages persisted in subSession by end of burst", len(dbMsgs))
	syncDo(t, p, func(u *UI) {
		t.Logf("diagnostic: subAgentSessionID=%q subAgentChat items=%d",
			u.subAgentSessionID, u.subAgentChat.Len())
	})

	grown := peak - baseline
	t.Logf("goroutines: baseline=%d peak=%d grown=%d", baseline, peak, grown)

	// The burst pushed ~600 real events through the system while up to
	// five spinners animated across two chats showing the same tool
	// calls. With animation frames scheduled solely by the UI's single
	// clock, transient goroutines (per-Cmd dispatch, message debounce)
	// come and go but never accumulate: peak growth stays around a few
	// dozen. The multiplying-tick-chain regression this guards against
	// is not subtle — it grew by thousands within seconds under this
	// exact load (and by 1.36M in the live incident) — so the bound
	// below has plenty of headroom against scheduler noise while still
	// catching any reintroduced feedback loop.
	require.Lessf(t, grown, 600,
		"goroutine count grew by %d (baseline=%d peak=%d) under the "+
			"sub-agent view + bash burst load — growth on the order of "+
			"the event volume or beyond means animation ticks are "+
			"multiplying again (the freeze this test guards against); "+
			"animation frames must only ever be scheduled by the UI's "+
			"single animation clock",
		grown, baseline, peak)
}
