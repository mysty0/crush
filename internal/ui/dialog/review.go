package dialog

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/diff"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

// ReviewID is the identifier for the review dialog.
const ReviewID = "review"

// reviewSplitModeMinWidth is the minimum content width at which the
// review dialog defaults to side-by-side (split) diffs.
const reviewSplitModeMinWidth = 140

// reviewHorizontalScrollStep is the number of columns scrolled per
// left/right key or wheel event.
const reviewHorizontalScrollStep = 5

// reviewFile is one changed file's first and latest recorded content in
// the session, along with the addition/deletion counts between them.
type reviewFile struct {
	path                 string
	before, after        string
	additions, deletions int
}

// Review is a fullscreen dialog that shows every file the agent changed
// in a session as a single scrollable diff, similar to `git diff` for
// the whole session.
type Review struct {
	com   *common.Common
	files []reviewFile

	viewport      viewport.Model
	viewportDirty bool
	splitMode     *bool // nil means use the width-based default
	xOffset       int
	// fileOffsets holds, for each file in files (same order), the
	// 0-indexed line number within the rendered content where that
	// file's diff block begins. Rebuilt whenever content is re-rendered.
	// Powers PrevFile/NextFile jumps.
	fileOffsets []int

	help   help.Model
	keyMap reviewKeyMap
}

type reviewKeyMap struct {
	ToggleDiffMode key.Binding
	ScrollUp       key.Binding
	ScrollDown     key.Binding
	ScrollLeft     key.Binding
	ScrollRight    key.Binding
	PrevFile       key.Binding
	NextFile       key.Binding
	PageUp         key.Binding
	PageDown       key.Binding
	Scroll         key.Binding
	Close          key.Binding
}

func defaultReviewKeyMap() reviewKeyMap {
	return reviewKeyMap{
		ToggleDiffMode: key.NewBinding(
			key.WithKeys("t"),
			key.WithHelp("t", "toggle diff view"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "scroll down"),
		),
		ScrollLeft: key.NewBinding(
			key.WithKeys("left", "h", "shift+left", "H"),
			key.WithHelp("←/h", "scroll left"),
		),
		ScrollRight: key.NewBinding(
			key.WithKeys("right", "l", "shift+right", "L"),
			key.WithHelp("→/l", "scroll right"),
		),
		PrevFile: key.NewBinding(
			key.WithKeys("shift+up", "K"),
			key.WithHelp("shift+↑/K", "prev file"),
		),
		NextFile: key.NewBinding(
			key.WithKeys("shift+down", "J"),
			key.WithHelp("shift+↓/J", "next file"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup", "b"),
			key.WithHelp("b/pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown", " ", "f"),
			key.WithHelp("f/pgdn", "page down"),
		),
		Scroll: key.NewBinding(
			key.WithKeys("up", "down", "left", "right"),
			key.WithHelp("↑↓←→", "scroll"),
		),
		Close: CloseKey,
	}
}

var _ Dialog = (*Review)(nil)

// NewReview creates a review dialog listing every file changed in the
// given session, diffing each file's first recorded version against its
// latest. Files with no net changes (e.g. a write that reproduced the
// original content) are omitted.
func NewReview(com *common.Common, sessionID string) (*Review, error) {
	hist, err := com.Workspace.ListSessionHistory(context.TODO(), sessionID)
	if err != nil {
		return nil, err
	}

	byPath := make(map[string][]history.File)
	for _, f := range hist {
		byPath[f.Path] = append(byPath[f.Path], f)
	}

	var files []reviewFile
	for path, versions := range byPath {
		first, last := versions[0], versions[0]
		for _, v := range versions {
			if v.Version < first.Version {
				first = v
			}
			if v.Version > last.Version {
				last = v
			}
		}
		_, additions, deletions := diff.GenerateDiff(first.Content, last.Content, path)
		if additions == 0 && deletions == 0 {
			continue
		}
		files = append(files, reviewFile{
			path:      path,
			before:    first.Content,
			after:     last.Content,
			additions: additions,
			deletions: deletions,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()

	km := defaultReviewKeyMap()
	vp := viewport.New()
	vp.KeyMap = viewport.KeyMap{
		Up:       km.ScrollUp,
		Down:     km.ScrollDown,
		Left:     km.ScrollLeft,
		Right:    km.ScrollRight,
		PageUp:   km.PageUp,
		PageDown: km.PageDown,
		// Disable other viewport keys to avoid conflicts with dialog
		// shortcuts (file-jump owns shift+up/down).
		HalfPageUp:   key.NewBinding(key.WithDisabled()),
		HalfPageDown: key.NewBinding(key.WithDisabled()),
	}

	return &Review{
		com:           com,
		files:         files,
		viewport:      vp,
		viewportDirty: true,
		help:          h,
		keyMap:        km,
	}, nil
}

// ID implements [Dialog].
func (*Review) ID() string {
	return ReviewID
}

// HasFiles reports whether there is anything to review.
func (r *Review) HasFiles() bool {
	return len(r.files) > 0
}

func (r *Review) isSplitMode() bool {
	if r.splitMode != nil {
		return *r.splitMode
	}
	return r.viewport.Width() >= reviewSplitModeMinWidth
}

// jumpToFile moves the viewport to the nearest file boundary in the
// given direction relative to the current scroll position: direction
// > 0 jumps to the next file's diff, direction < 0 to the previous
// one. No-op if there is nowhere to go in that direction.
func (r *Review) jumpToFile(direction int) {
	if len(r.fileOffsets) == 0 {
		return
	}
	current := r.viewport.YOffset()
	if direction > 0 {
		for _, off := range r.fileOffsets {
			if off > current {
				r.viewport.SetYOffset(off)
				return
			}
		}
		return
	}
	target := -1
	for _, off := range r.fileOffsets {
		if off >= current {
			break
		}
		target = off
	}
	if target >= 0 {
		r.viewport.SetYOffset(target)
	}
}

// HandleMsg implements [Dialog].
func (r *Review) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, r.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, r.keyMap.ToggleDiffMode):
			newMode := !r.isSplitMode()
			r.splitMode = &newMode
			r.viewportDirty = true
		case key.Matches(msg, r.keyMap.ScrollLeft):
			r.xOffset = max(0, r.xOffset-reviewHorizontalScrollStep)
			r.viewportDirty = true
		case key.Matches(msg, r.keyMap.ScrollRight):
			r.xOffset += reviewHorizontalScrollStep
			r.viewportDirty = true
		case key.Matches(msg, r.keyMap.PrevFile):
			r.jumpToFile(-1)
		case key.Matches(msg, r.keyMap.NextFile):
			r.jumpToFile(1)
		default:
			r.viewport, _ = r.viewport.Update(msg)
		}
	case common.CoalescedWheelMsg:
		switch {
		case msg.DeltaX < 0:
			r.xOffset = max(0, r.xOffset-reviewHorizontalScrollStep)
			r.viewportDirty = true
		case msg.DeltaX > 0:
			r.xOffset += reviewHorizontalScrollStep
			r.viewportDirty = true
		default:
			r.viewport, _ = r.viewport.Update(tea.MouseWheelMsg(msg.Mouse))
		}
	}
	return nil
}

// Draw implements [Dialog]. The review dialog always takes up nearly the
// whole window — there is no compact mode, since the point is to see
// every change at once.
func (r *Review) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := r.com.Styles

	width := area.Dx()
	maxHeight := area.Dy()

	dialogStyle := t.Dialog.View.Width(width).Padding(0, 1)
	contentWidth := width - t.Dialog.View.GetHorizontalFrameSize() - 2

	header := r.renderHeader(contentWidth)
	helpView := r.help.View(r)

	headerHeight := lipgloss.Height(header)
	helpHeight := lipgloss.Height(helpView)
	const layoutSpacingLines = 4
	frameHeight := dialogStyle.GetVerticalFrameSize() + layoutSpacingLines

	availableHeight := max(maxHeight-headerHeight-helpHeight-frameHeight, 3)
	viewportWidth := contentWidth - 1 // Reserve space for scrollbar.

	if r.viewport.Width() != viewportWidth {
		r.viewportDirty = true
	}
	r.viewport.SetWidth(viewportWidth)
	r.viewport.SetHeight(availableHeight)
	if r.viewportDirty {
		r.viewport.SetContent(r.renderContent(viewportWidth))
		r.viewportDirty = false
	}

	content := r.viewport.View()
	scrollbar := common.Scrollbar(t, availableHeight, r.viewport.TotalLineCount(), availableHeight, r.viewport.YOffset())
	content = lipgloss.JoinHorizontal(lipgloss.Top, content, scrollbar)

	parts := []string{header, "", content, "", helpView}
	innerContent := lipgloss.JoinVertical(lipgloss.Left, parts...)
	DrawCenterCursor(scr, area, dialogStyle.Render(innerContent), nil)
	return nil
}

func (r *Review) renderHeader(contentWidth int) string {
	t := r.com.Styles
	title := common.DialogTitle(t, "Review Changes",
		contentWidth-t.Dialog.Title.GetHorizontalFrameSize(),
		t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)
	title = t.Dialog.Title.Render(title)

	var additions, deletions int
	for _, f := range r.files {
		additions += f.additions
		deletions += f.deletions
	}
	summary := fmt.Sprintf("%d file(s) changed, +%d -%d", len(r.files), additions, deletions)
	summaryLine := t.Dialog.Permissions.ValueText.Render(summary)

	return lipgloss.JoinVertical(lipgloss.Left, title, "", summaryLine)
}

func (r *Review) renderContent(width int) string {
	isSplit := r.isSplitMode()
	r.fileOffsets = make([]int, 0, len(r.files))
	var b strings.Builder
	lineOffset := 0
	for i, f := range r.files {
		r.fileOffsets = append(r.fileOffsets, lineOffset)
		formatter := common.DiffFormatter(r.com.Styles).
			Before(fsext.PrettyPath(f.path), f.before).
			After(fsext.PrettyPath(f.path), f.after).
			FileName(fsext.PrettyPath(f.path)).
			XOffset(r.xOffset).
			Width(width)
		if isSplit {
			formatter = formatter.Split()
		} else {
			formatter = formatter.Unified()
		}
		formatted := formatter.String()
		b.WriteString(formatted)
		lineOffset += strings.Count(formatted, "\n")
		if i < len(r.files)-1 {
			b.WriteString("\n\n")
			lineOffset += 2
		}
	}
	return b.String()
}

// ShortHelp implements [help.KeyMap].
func (r *Review) ShortHelp() []key.Binding {
	return []key.Binding{
		r.keyMap.Scroll,
		r.keyMap.PageDown,
		r.keyMap.NextFile,
		r.keyMap.ToggleDiffMode,
		r.keyMap.Close,
	}
}

// FullHelp implements [help.KeyMap].
func (r *Review) FullHelp() [][]key.Binding {
	return [][]key.Binding{r.ShortHelp()}
}
