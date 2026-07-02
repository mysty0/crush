package list

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Item represents a single item in the lazy-loaded list.
//
// Items participate in the list-level render memo (F6). The cache key
// for each item is (pointer, width, version). Items must:
//
//   - Bump their version (via the embedded *Versioned helper) on every
//     mutation that changes the rendered output.
//   - Return Finished() == true once their rendered output will not
//     change again unless an explicit mutator is invoked. Frozen
//     entries are emitted verbatim — no Render call — until either
//     Version() bumps, the viewport width changes, or the list
//     explicitly invalidates the entry.
type Item interface {
	// Render returns the string representation of the item for the given
	// width.
	Render(width int) string

	// Version returns a monotonic counter that the list-level cache
	// uses to detect mutations. Items must increment the version
	// (via Versioned.Bump) on every state change that would alter
	// the rendered output.
	Version() uint64

	// Finished reports whether the item's rendered output has reached
	// a terminal state and may be frozen by the list cache. Items
	// that animate, stream, or otherwise still mutate must return
	// false. A finished item that later mutates must bump its
	// version on the mutation; the cache treats version bumps as
	// implicit unfreeze + invalidate.
	Finished() bool
}

// HeightEstimator is an optional interface for items that can cheaply
// estimate their rendered height without performing a full (expensive)
// Render. The list uses this for scrollbar math (TotalHeight/Offset) so
// that opening a long conversation does not have to render every
// off-screen item up front. The estimate need not be exact — it is
// replaced by the true height as soon as the item is actually rendered
// (i.e. scrolled into view). Visible items are always rendered exactly.
type HeightEstimator interface {
	// EstimatedHeight returns an approximate rendered height in lines
	// for the given width. Cheaper is better; correctness of the
	// scrollbar improves as items are scrolled into view.
	EstimatedHeight(width int) int
}

// Versioned is a tiny embeddable helper that satisfies Item.Version()
// and provides a Bump() method to call from every state-mutating
// method. Items typically embed *Versioned alongside their other
// helpers; see chat.AssistantMessageItem for the canonical wiring.
//
// Bump() is not safe for concurrent use; callers must hold whatever
// synchronization their item type already requires for state
// mutations. The list itself never reads Version() from a goroutine
// other than the UI thread.
type Versioned struct {
	v uint64
}

// NewVersioned returns a fresh *Versioned at version zero.
func NewVersioned() *Versioned {
	return &Versioned{}
}

// Version returns the current version counter.
func (vc *Versioned) Version() uint64 {
	return vc.v
}

// Bump advances the version counter by one. Mutators on items that
// affect the rendered output must call Bump exactly once per
// observable state change. Bumping more than once per change is
// harmless other than a single extra cache miss.
func (vc *Versioned) Bump() {
	vc.v++
}

// RawRenderable represents an item that can provide a raw rendering
// without additional styling.
type RawRenderable interface {
	// RawRender returns the raw rendered string without any additional
	// styling.
	RawRender(width int) string
}

// Focusable represents an item that can be aware of focus state changes.
type Focusable interface {
	// SetFocused sets the focus state of the item.
	SetFocused(focused bool)
}

// Highlightable represents an item that can highlight a portion of its content.
type Highlightable interface {
	// SetHighlight highlights the content from the given start to end
	// positions. Use -1 for no highlight.
	SetHighlight(startLine, startCol, endLine, endCol int)
	// Highlight returns the current highlight positions within the item.
	Highlight() (startLine, startCol, endLine, endCol int)
}

// MouseClickable represents an item that can handle mouse click events.
type MouseClickable interface {
	// HandleMouseClick processes a mouse click event at the given coordinates.
	// It returns true if the event was handled, false otherwise.
	HandleMouseClick(btn ansi.MouseButton, x, y int) bool
}

// SpacerItem is a spacer item that adds vertical space in the list.
type SpacerItem struct {
	*Versioned
	Height int
}

// NewSpacerItem creates a new [SpacerItem] with the specified height.
func NewSpacerItem(height int) *SpacerItem {
	return &SpacerItem{
		Versioned: NewVersioned(),
		Height:    max(0, height-1),
	}
}

// Render implements the Item interface for [SpacerItem].
func (s *SpacerItem) Render(width int) string {
	return strings.Repeat("\n", s.Height)
}

// Finished implements Item. SpacerItems are immutable in practice and
// safe to freeze; any mutation goes through Versioned.Bump which
// invalidates the frozen entry.
func (s *SpacerItem) Finished() bool {
	return true
}
