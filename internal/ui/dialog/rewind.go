package dialog

import (
	"context"
	"fmt"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/rewind"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	uv "github.com/charmbracelet/ultraviolet"
)

// RewindID is the identifier for the rewind dialog.
const (
	RewindID             = "rewind"
	rewindDialogMaxWidth = 72
)

// rewindPhase is the two-step flow of the rewind dialog.
type rewindPhase uint8

const (
	// rewindPhasePoints lists the user messages to rewind to.
	rewindPhasePoints rewindPhase = iota
	// rewindPhaseMode picks what to restore for the chosen point.
	rewindPhaseMode
)

// Rewind is a two-step dialog: pick a point in the conversation, then pick
// what to restore (conversation, code, or both). It forks the session so
// the original is never lost.
type Rewind struct {
	com       *common.Common
	help      help.Model
	list      *list.FilterableList
	sessionID string

	phase           rewindPhase
	points          []rewind.RewindPoint
	selectedMessage string

	keyMap struct {
		Select   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Back     key.Binding
		Close    key.Binding
	}
}

var _ Dialog = (*Rewind)(nil)

// NewRewind creates a rewind dialog for the given session, loading its
// rewind points up front.
func NewRewind(com *common.Common, sessionID string) (*Rewind, error) {
	points, err := com.Workspace.RewindListPoints(context.TODO(), sessionID)
	if err != nil {
		return nil, err
	}

	r := &Rewind{
		com:       com,
		sessionID: sessionID,
		phase:     rewindPhasePoints,
		points:    points,
	}

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	r.help = h

	r.list = list.NewFilterableList()
	r.list.Focus()

	r.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "choose"),
	)
	r.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	r.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	r.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑↓", "choose"),
	)
	r.keyMap.Back = key.NewBinding(
		key.WithKeys("left"),
		key.WithHelp("←", "back"),
	)
	r.keyMap.Close = CloseKey

	r.setPointItems()
	return r, nil
}

// ID implements [Dialog].
func (r *Rewind) ID() string {
	return RewindID
}

// setPointItems fills the list with one entry per rewind point.
func (r *Rewind) setPointItems() {
	items := make([]list.FilterableItem, 0, len(r.points))
	for _, p := range r.points {
		info := relativeTime(p.CreatedAt)
		if p.FilesChanged > 0 {
			info = fmt.Sprintf("%d file%s · %s", p.FilesChanged, plural(p.FilesChanged), info)
		}
		title := p.Preview
		if title == "" {
			title = "(empty message)"
		}
		items = append(items, NewCommandItem(
			r.com.Styles, "point_"+p.MessageID, title, info,
			ActionRewindSelectPoint{MessageID: p.MessageID},
		))
	}
	r.list.SetItems(items...)
	r.list.ScrollToTop()
	r.list.SetSelected(0)
}

// setModeItems fills the list with the restore-mode choices for the
// selected point.
func (r *Rewind) setModeItems() {
	mk := func(mode rewind.Mode, title, desc string) list.FilterableItem {
		return NewCommandItem(
			r.com.Styles, "mode_"+string(mode), title, "",
			ActionRewindConfirm{SessionID: r.sessionID, MessageID: r.selectedMessage, Mode: mode},
		).WithDescription(desc)
	}
	r.list.SetItems(
		mk(rewind.ModeBoth, "Restore code and conversation", "Fork the conversation and roll back edited files on disk."),
		mk(rewind.ModeConversation, "Restore conversation", "Fork the conversation; leave files on disk unchanged."),
		mk(rewind.ModeCode, "Restore code", "Roll back edited files on disk; keep the conversation as is."),
	)
	r.list.ScrollToTop()
	r.list.SetSelected(0)
}

// HandleMsg implements [Dialog].
func (r *Rewind) HandleMsg(msg tea.Msg) Action {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch {
	case key.Matches(keyMsg, r.keyMap.Close):
		return ActionClose{}
	case key.Matches(keyMsg, r.keyMap.Back):
		if r.phase == rewindPhaseMode {
			r.phase = rewindPhasePoints
			r.selectedMessage = ""
			r.setPointItems()
		}
		return nil
	case key.Matches(keyMsg, r.keyMap.Previous):
		r.list.Focus()
		if r.list.IsSelectedFirst() {
			r.list.SelectLast()
		} else {
			r.list.SelectPrev()
		}
		r.list.ScrollToSelected()
		return nil
	case key.Matches(keyMsg, r.keyMap.Next):
		r.list.Focus()
		if r.list.IsSelectedLast() {
			r.list.SelectFirst()
		} else {
			r.list.SelectNext()
		}
		r.list.ScrollToSelected()
		return nil
	case key.Matches(keyMsg, r.keyMap.Select):
		selected := r.list.SelectedItem()
		item, ok := selected.(*CommandItem)
		if !ok || item == nil {
			return nil
		}
		action := item.Action()
		// Intercept point selection to advance to the mode step; the mode
		// selection action bubbles up to the UI.
		if sel, ok := action.(ActionRewindSelectPoint); ok {
			r.selectedMessage = sel.MessageID
			r.phase = rewindPhaseMode
			r.setModeItems()
			return nil
		}
		return action
	}
	return nil
}

// Draw implements [Dialog].
func (r *Rewind) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := r.com.Styles
	width := max(0, min(rewindDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()

	r.list.SetSize(innerWidth, max(1, height-heightOffset))
	r.help.SetWidth(innerWidth)

	rc := NewRenderContext(t, width)
	switch r.phase {
	case rewindPhaseMode:
		rc.Title = "Rewind — What to restore"
	default:
		rc.Title = "Rewind"
	}

	if len(r.points) == 0 {
		rc.AddPart(t.Dialog.SecondaryText.Render("Nothing to rewind to yet."))
	} else {
		listView := t.Dialog.List.Height(r.list.Height()).Render(r.list.Render())
		rc.AddPart(listView)
		if r.phase == rewindPhaseMode {
			rc.AddPart(t.Dialog.SecondaryText.Render(
				"Only files edited by Crush's tools can be restored. The original session is kept.",
			))
		}
	}
	rc.Help = r.help.View(r)

	DrawCenter(scr, area, rc.Render())
	return nil
}

// ShortHelp implements [help.KeyMap].
func (r *Rewind) ShortHelp() []key.Binding {
	if r.phase == rewindPhaseMode {
		return []key.Binding{r.keyMap.UpDown, r.keyMap.Select, r.keyMap.Back, r.keyMap.Close}
	}
	return []key.Binding{r.keyMap.UpDown, r.keyMap.Select, r.keyMap.Close}
}

// FullHelp implements [help.KeyMap].
func (r *Rewind) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{r.keyMap.Select, r.keyMap.Next, r.keyMap.Previous},
		{r.keyMap.Back, r.keyMap.Close},
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// relativeTime renders a Unix timestamp as a short "5m ago" style string.
func relativeTime(ts int64) string {
	if ts <= 0 {
		return ""
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}
