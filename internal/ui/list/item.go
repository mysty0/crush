package list

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Item represents a single item in the lazy-loaded list.
//
// Items participate in the list-level render memo (F6). The cache key
// for each item is (pointer, width, paint version). Items must:
//
//   - Bump their paint version (via the embedded *Versioned helper) on
//     every mutation that changes the rendered output.
//   - Bump their layout version (via BumpLayout) whenever a mutation
//     may change the item's rendered height, not just its pixels.
//   - Return Finished() == true once their rendered output will not
//     change again unless an explicit mutator is invoked. Frozen
//     entries are emitted verbatim — no Render call — until either
//     PaintVersion() bumps, the viewport width changes, or the list
//     explicitly invalidates the entry.
type Item interface {
	// Render returns the string representation of the item for the given
	// width.
	Render(width int) string

	// PaintVersion returns a monotonic counter that the list-level
	// cache uses to detect changes to the rendered output. Items must
	// increment it (via Versioned.Bump or Versioned.BumpLayout) on
	// every state change that would alter the rendered bytes, whether
	// or not the height changes.
	PaintVersion() uint64

	// LayoutVersion returns a monotonic counter that the list uses to
	// detect changes to the rendered height. Items must increment it
	// (via Versioned.BumpLayout) on every state change that may alter
	// the number of rendered lines. A paint-only change (Bump) leaves
	// LayoutVersion untouched, which is what lets the height oracle
	// keep a cached height across animation frames without rendering.
	LayoutVersion() uint64

	// Finished reports whether the item's rendered output has reached
	// a terminal state and may be frozen by the list cache. Items
	// that animate, stream, or otherwise still mutate must return
	// false. A finished item that later mutates must bump its
	// version on the mutation; the cache treats version bumps as
	// implicit unfreeze + invalidate.
	Finished() bool
}

// Versioned is a tiny embeddable helper that satisfies the Item
// version methods and provides Bump/BumpLayout to call from every
// state-mutating method. Items typically embed *Versioned alongside
// their other helpers; see chat.AssistantMessageItem for the
// canonical wiring.
//
// It carries two counters so the list can tell "pixels changed" apart
// from "geometry may have changed":
//
//   - paint advances on any change to the rendered bytes.
//   - layout advances only when the change may alter the item's
//     rendered height (line count).
//
// The split exists because the list caches off-screen item heights and
// must not throw them away for a paint-only mutation. A spinner frame
// advancing at 20fps is a paint bump; the item's height is unchanged,
// so its cached height (and therefore scroll anchoring against it)
// survives indefinitely. The misclassification cost is asymmetric: an
// over-eager BumpLayout merely forces one redundant render, while a
// Bump can NEVER corrupt geometry because it leaves the layout
// counter — the only thing the height oracle trusts — untouched. When
// in doubt, prefer BumpLayout.
//
// Neither method is safe for concurrent use; callers must hold
// whatever synchronization their item type already requires for state
// mutations. The list itself never reads the counters from a
// goroutine other than the UI thread.
type Versioned struct {
	paint  uint64
	layout uint64
}

// NewVersioned returns a fresh *Versioned with both counters at zero.
func NewVersioned() *Versioned {
	return &Versioned{}
}

// PaintVersion returns the current paint counter.
func (vc *Versioned) PaintVersion() uint64 {
	return vc.paint
}

// LayoutVersion returns the current layout counter.
func (vc *Versioned) LayoutVersion() uint64 {
	return vc.layout
}

// Bump advances the paint counter only: the rendered output changed
// but the item's height did not (e.g. a spinner frame or focus
// highlight). Because the height oracle keys off the layout counter,
// a Bump never invalidates a cached height. Mutators must call it
// exactly once per observable paint-only change.
func (vc *Versioned) Bump() {
	vc.paint++
}

// BumpLayout advances BOTH counters: the change may alter the item's
// rendered height (new text, results, nested tools, expansion, ...).
// A layout change always implies a paint change, so the paint counter
// moves too and the cached content is invalidated alongside the
// cached height. Mutators must call it exactly once per observable
// height-affecting change.
func (vc *Versioned) BumpLayout() {
	vc.paint++
	vc.layout++
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
