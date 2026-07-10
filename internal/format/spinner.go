package format

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/x/ansi"
)

// Spinner wraps the bubbles spinner for non-interactive mode
type Spinner struct {
	done chan struct{}
	prog *tea.Program
}

// frameMsg drives this standalone spinner's animation. This program
// owns exactly one Anim, so a single self-rescheduling tick chain is
// safe here.
type frameMsg struct{}

func frameCmd() tea.Cmd {
	return tea.Tick(anim.Interval(), func(time.Time) tea.Msg { return frameMsg{} })
}

type model struct {
	cancel context.CancelFunc
	anim   *anim.Anim
}

func (m model) Init() tea.Cmd  { return frameCmd() }
func (m model) View() tea.View { return tea.NewView(m.anim.Render()) }

// Update implements tea.Model.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.cancel()
			return m, tea.Quit
		}
	case frameMsg:
		m.anim.Advance()
		return m, frameCmd()
	}
	return m, nil
}

// NewSpinner creates a new spinner with the given message
func NewSpinner(ctx context.Context, cancel context.CancelFunc, animSettings anim.Settings) *Spinner {
	m := model{
		anim:   anim.New(animSettings),
		cancel: cancel,
	}

	p := tea.NewProgram(m, tea.WithOutput(os.Stderr), tea.WithContext(ctx))

	return &Spinner{
		prog: p,
		done: make(chan struct{}, 1),
	}
}

// Start begins the spinner animation
func (s *Spinner) Start() {
	go func() {
		defer close(s.done)
		_, err := s.prog.Run()
		// ensures line is cleared
		fmt.Fprint(os.Stderr, ansi.EraseEntireLine)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, tea.ErrInterrupted) {
			fmt.Fprintf(os.Stderr, "Error running spinner: %v\n", err)
		}
	}()
}

// Stop ends the spinner animation
func (s *Spinner) Stop() {
	s.prog.Quit()
	<-s.done
}
