package app

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/notify"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
)

// NewForTest constructs a minimal [App] suitable for in-process tests
// that need a working event broker and permission service without
// booting a real config, database, LSP, MCP, or agent coordinator.
//
// The returned App has:
//
//   - A live `events` broker that [App.SendEvent] publishes to and
//     [App.Events] subscribes from.
//   - A real [permission.Service] whose request and notification
//     brokers are fanned into the events broker, so subscribers to
//     [App.Events] observe the same permission events the production
//     wiring would deliver to SSE clients.
//   - An [App.agentNotifications] broker.
//
// The caller owns lifetime: cancel ctx (or call [App.Shutdown]) to
// tear down the fan-in goroutines and the events broker.
func NewForTest(ctx context.Context) *App {
	app := &App{
		Permissions:        permission.NewPermissionService("", false, nil),
		globalCtx:          ctx,
		events:             pubsub.NewBroker[tea.Msg](),
		serviceEventsWG:    &sync.WaitGroup{},
		tuiWG:              &sync.WaitGroup{},
		agentNotifications: pubsub.NewBroker[notify.Notification](),
		runCompletions:     pubsub.NewBroker[notify.RunComplete](),
	}

	eventsCtx, cancel := context.WithCancel(ctx)
	app.eventsCtx = eventsCtx
	setupSubscriber(eventsCtx, app.serviceEventsWG, "permissions",
		app.Permissions.Subscribe, app.events)
	setupSubscriber(eventsCtx, app.serviceEventsWG, "permissions-notifications",
		app.Permissions.SubscribeNotifications, app.events)
	setupSubscriber(eventsCtx, app.serviceEventsWG, "agent-notifications",
		app.agentNotifications.Subscribe, app.events)
	setupSubscriber(eventsCtx, app.serviceEventsWG, "run-completions",
		app.runCompletions.Subscribe, app.events)
	app.cleanupFuncs = append(app.cleanupFuncs, func(context.Context) error {
		cancel()
		app.serviceEventsWG.Wait()
		app.events.Shutdown()
		return nil
	})
	return app
}

// NewForTestWithSessions extends [NewForTest] with real session and
// message services wired into the events broker exactly as
// production's setupEvents does, plus the bash/workflow progress
// brokers. Use this when a test needs a [tea.Program] subscribed via
// [App.Subscribe] to observe real session, message, and tool-progress
// events — e.g. reproducing UI behavior driven by a live streaming
// session — without booting a full config, LSP, MCP, or agent
// coordinator.
//
// The caller supplies sessions and messages (typically backed by a
// real, temporary SQLite DB — see internal/db.Connect) since both are
// stateful services with their own persistence and debounce behavior
// that this helper does not construct.
func NewForTestWithSessions(ctx context.Context, sessions session.Service, messages message.Service) *App {
	a := NewForTest(ctx)
	a.Sessions = sessions
	a.Messages = messages
	setupSubscriber(a.eventsCtx, a.serviceEventsWG, "sessions", sessions.Subscribe, a.events)
	setupSubscriber(a.eventsCtx, a.serviceEventsWG, "messages", messages.Subscribe, a.events)
	setupSubscriber(a.eventsCtx, a.serviceEventsWG, "bash-progress", agenttools.SubscribeBashProgress, a.events)
	setupSubscriber(a.eventsCtx, a.serviceEventsWG, "workflow-progress", agenttools.SubscribeWorkflowProgress, a.events)
	setupSubscriber(a.eventsCtx, a.serviceEventsWG, "workflow-status", agenttools.SubscribeWorkflowStatus, a.events)
	setupSubscriber(a.eventsCtx, a.serviceEventsWG, "retry-progress", agent.SubscribeRetryProgress, a.events)
	return a
}

// ShutdownForTest tears down the App's event broker and fan-in
// goroutines. It is safe to call multiple times.
//
// Use this in tests instead of [App.Shutdown], which drives a full
// production shutdown path (database release, LSP teardown, MCP
// shutdown) that synthetic test apps cannot satisfy.
func (app *App) ShutdownForTest() {
	for _, cleanup := range app.cleanupFuncs {
		if cleanup != nil {
			_ = cleanup(context.Background())
		}
	}
	app.cleanupFuncs = nil
}
