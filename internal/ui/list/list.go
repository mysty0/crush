package list

import (
	"strings"
)

// List represents a list of items that can be lazily rendered. A list is
// always rendered like a chat conversation where items are stacked vertically
// from top to bottom.
type List struct {
	// Viewport size
	width, height int

	// Items in the list
	items []Item

	// Gap between items (0 or less means no gap)
	gap int

	// show list in reverse order
	reverse bool

	// Focus and selection state
	focused     bool
	selectedIdx int // The current selected index -1 means no selection

	// offsetIdx is the index of the first visible item in the viewport.
	// offsetLine is the number of lines of the item at offsetIdx that
	// are scrolled out of view (above the viewport); it must always be
	// >= 0.
	//
	// When bottomAnchored is true these two fields are DERIVED, not
	// authoritative: Render (and any offset consumer) recomputes them
	// from lastOffsetItem() with exact heights before use. See
	// bottomAnchored below.
	offsetIdx  int
	offsetLine int

	// bottomAnchored glues the viewport to the end of the content:
	// rendering hangs content upward from the last line. While true,
	// offsetIdx/offsetLine are recomputed from the true (exact-height)
	// bottom rather than compared against it, which makes "kicked away
	// from the bottom by stale height math" structurally impossible —
	// the anchor IS the bottom, so there is nothing to lose a race
	// with. It is set by ScrollToBottom and by ScrollBy's down-clamp
	// (reaching the true bottom re-anchors instead of fighting the
	// user) and cleared by any scroll that deliberately detaches from
	// the end. Reverse-mode lists never set it; see SetReverse.
	bottomAnchored bool

	// renderCallbacks is a list of callbacks to apply when rendering items.
	renderCallbacks []func(idx, selectedIdx int, item Item) Item

	// totalHeightCache is a cached value of the total rendered height of
	// all items. It is invalidated whenever the item set changes or the
	// viewport width changes (which can alter per-item line counts).
	totalHeightCache int
	totalHeightValid bool

	// cache is the F6 list-level render memo, keyed by item pointer.
	// Each entry stores the rendered content, a pre-split slice of
	// lines (so AtBottom / Render / VisibleItemIndices /
	// findItemAtY all share one render per frame), the height, and
	// the keys that govern invalidation (width and version). The
	// frozen flag mirrors §4.5.1: once a Finished() item is
	// rendered, subsequent draws return the stored output verbatim
	// without calling back into Render.
	cache map[Item]*listCacheEntry

	// freezeSuppressed marks items the list must not freeze on the
	// next render even when their Finished() reports true. This is
	// the §4.5.1 selection-drag escape hatch (option (a)): items
	// inside an active selection range render as live items so that
	// per-line highlight overlays land on the latest content. Cleared
	// on EndSelectionDrag.
	freezeSuppressed map[Item]struct{}
}

// listCacheEntry is the per-item entry in the list-level render memo.
// It records both version counters at render time: paintV governs
// content validity (the cached bytes are correct only while the
// item's paint version is unchanged) and layoutV governs height
// validity (the cached height survives paint-only bumps and is only
// stale once the item's layout version moves). See heightExact.
type listCacheEntry struct {
	width   int
	paintV  uint64
	layoutV uint64
	frozen  bool
	content string
	lines   []string
	height  int
}

// renderedItem is the legacy view of a cached entry returned by getItem.
// Internal callers that don't need the line slice keep using this
// shape; functions that walk lines (Render) take the slice off the
// cache entry directly.
type renderedItem struct {
	content string
	height  int
}

// NewList creates a new lazy-loaded list.
func NewList(items ...Item) *List {
	l := new(List)
	l.items = items
	l.selectedIdx = -1
	l.cache = make(map[Item]*listCacheEntry)
	l.freezeSuppressed = make(map[Item]struct{})
	return l
}

// RenderCallback defines a function that can modify an item before it is
// rendered.
type RenderCallback func(idx, selectedIdx int, item Item) Item

// RegisterRenderCallback registers a callback to be called when rendering
// items. This can be used to modify items before they are rendered.
func (l *List) RegisterRenderCallback(cb RenderCallback) {
	l.renderCallbacks = append(l.renderCallbacks, cb)
}

// SetSize sets the size of the list viewport. A width change drops the
// entire render cache because every entry's wrapped output depends on
// width; a height-only change is a no-op for the cache.
func (l *List) SetSize(width, height int) {
	if l.width != width {
		l.invalidateAll()
	}
	l.width = width
	l.height = height
}

// SetGap sets the gap between items.
func (l *List) SetGap(gap int) {
	l.gap = gap
}

// Gap returns the gap between items.
func (l *List) Gap() int {
	return l.gap
}

// AtBottom returns whether the list is showing the last item at the
// bottom.
//
// When bottomAnchored, the answer is definitionally yes: the viewport
// is glued to the end. It is also yes when the entire content fits in
// the viewport. Otherwise it walks the visible range with EXACT
// heights (heightExact) — the whole point of the version split is that
// this comparison must never be made against a stale height, or a user
// scrolled to the true bottom would be told they are not there.
func (l *List) AtBottom() bool {
	if len(l.items) == 0 {
		return true
	}
	if l.bottomAnchored {
		return true
	}

	// Calculate the height from offsetIdx to the end, starting the
	// accumulator at -offsetLine so it already accounts for the lines of
	// the first item that are scrolled past. This keeps the early-exit
	// check below consistent with the final comparison.
	totalHeight := -l.offsetLine
	for idx := l.offsetIdx; idx < len(l.items); idx++ {
		if totalHeight > l.height {
			// No need to calculate further, we're already past the viewport height
			return false
		}
		itemHeight := l.heightExact(idx)
		if l.gap > 0 && idx > l.offsetIdx {
			itemHeight += l.gap
		}
		totalHeight += itemHeight
	}

	return totalHeight <= l.height
}

// SetReverse shows the list in reverse order.
//
// Reverse mode inverts ScrollBy's direction and flips the rendered
// lines. Bottom-anchored follow is deliberately NOT wired through
// reverse mode: "the bottom" is ambiguous once lines are flipped, and
// the only reverse consumer (the completions dropdown) is a small,
// fully-visible list that never needs follow. Turning reverse on
// therefore clears any anchor so reverse behavior stays byte-identical
// to before the follow-mode work.
func (l *List) SetReverse(reverse bool) {
	l.reverse = reverse
	if reverse {
		l.bottomAnchored = false
	}
}

// Width returns the width of the list viewport.
func (l *List) Width() int {
	return l.width
}

// Height returns the height of the list viewport.
func (l *List) Height() int {
	return l.height
}

// Len returns the number of items in the list.
func (l *List) Len() int {
	return len(l.items)
}

// TotalHeightApprox returns an APPROXIMATE total height of all items,
// suitable ONLY for presentation (scrollbar sizing, "does the content
// fit" checks). It must NEVER back a positioning decision: it renders
// nothing, so unrendered items contribute the heightApprox constant of
// 1 and the sum converges to the true total only as items are actually
// rendered into view. The result is cached and recomputed when the
// item set or viewport width changes.
func (l *List) TotalHeightApprox() int {
	if l.totalHeightValid {
		return l.totalHeightCache
	}
	total := 0
	for idx := range l.items {
		total += l.heightApprox(idx)
		if l.gap > 0 && idx < len(l.items)-1 {
			total += l.gap
		}
	}
	l.totalHeightCache = total
	l.totalHeightValid = true
	return total
}

// heightExact returns the item's true rendered height in lines. It is
// the single height oracle for every positioning decision.
//
// The hot path is the version check: if the item was rendered at this
// width and its LAYOUT version is unchanged, the cached height is
// returned WITHOUT rendering. This is what lets a spinning item
// advancing at 20fps (paint bumps only) keep a valid height
// indefinitely, so scroll anchoring never has to render an off-screen
// animating item just to learn how tall it is. Otherwise it renders
// via getItem (which repopulates the cache) and returns the true
// height. Positioning walks that call this are all O(viewport) — they
// early-exit after one screenful — so the worst case is rendering
// about one screen of items that were about to be rendered anyway.
func (l *List) heightExact(idx int) int {
	if idx < 0 || idx >= len(l.items) {
		return 0
	}
	rawItem := l.items[idx]
	if entry := l.cache[rawItem]; entry != nil && entry.width == l.width &&
		entry.layoutV == rawItem.LayoutVersion() {
		return entry.height
	}
	return l.getItem(idx).height
}

// heightApprox returns an APPROXIMATE height for scrollbar-only
// callers (TotalHeightApprox / OffsetApprox). It never renders: a
// cached entry with matching width at ANY version yields its stored
// height (a stale height is fine for a scrollbar), and everything else
// yields the constant 1. This must NEVER be used for positioning —
// its whole purpose is to avoid rendering off-screen items, so it
// knowingly returns wrong heights that converge as items render.
func (l *List) heightApprox(idx int) int {
	if idx < 0 || idx >= len(l.items) {
		return 0
	}
	rawItem := l.items[idx]
	if entry := l.cache[rawItem]; entry != nil && entry.width == l.width {
		return entry.height
	}
	return 1
}

// OffsetApprox returns an APPROXIMATE scroll offset in lines from the
// top, for scrollbar rendering only. Like TotalHeightApprox it uses
// heightApprox and must NEVER inform a positioning decision.
func (l *List) OffsetApprox() int {
	offset := 0
	for idx := 0; idx < l.offsetIdx; idx++ {
		offset += l.heightApprox(idx)
		if l.gap > 0 && idx < len(l.items)-1 {
			offset += l.gap
		}
	}
	offset += l.offsetLine
	return offset
}

// lastOffsetItem returns the index and line offsets of the last item
// that can be partially visible in the viewport, i.e. the exact
// scroll anchor that places the final line of content on the final
// line of the viewport.
//
// It walks backward from the end with EXACT heights (heightExact):
// this is the true bottom, and end-anchored follow derives its offsets
// from here. The walk is O(viewport) — it stops as soon as one
// screenful of content is accounted for — so at most one screen of
// not-yet-rendered items is rendered, all of which are about to be
// drawn anyway.
func (l *List) lastOffsetItem() (int, int, int) {
	var totalHeight int
	var idx int
	for idx = len(l.items) - 1; idx >= 0; idx-- {
		itemHeight := l.heightExact(idx)
		if l.gap > 0 && idx < len(l.items)-1 {
			itemHeight += l.gap
		}
		totalHeight += itemHeight
		if totalHeight > l.height {
			break
		}
	}

	// Calculate line offset within the item
	lineOffset := max(totalHeight-l.height, 0)
	idx = max(idx, 0)

	return idx, lineOffset, totalHeight
}

// getItem renders (if needed) and returns the item at the given index.
// The result is served from the F6 cache when possible — see
// renderItemEntry for the cache-key semantics.
func (l *List) getItem(idx int) renderedItem {
	if idx < 0 || idx >= len(l.items) {
		return renderedItem{}
	}
	entry := l.renderItemEntry(idx)
	if entry == nil {
		return renderedItem{}
	}
	return renderedItem{content: entry.content, height: entry.height}
}

// renderItemEntry returns the cache entry for the given index, populating
// the cache on miss. The result must not be retained past the next
// invalidation (SetSize width change, SetItems, etc.).
//
// Render callbacks always run, even for frozen entries: callbacks
// are how the list discovers per-frame state changes (selection,
// highlight range) and they bump the item's paint version when those
// changes affect the rendered output. A frozen item whose callback
// run is a no-op (same focus, same highlight) keeps its stored paint
// version and the cache hit is preserved on the post-callback paint
// version check.
func (l *List) renderItemEntry(idx int) *listCacheEntry {
	if idx < 0 || idx >= len(l.items) {
		return nil
	}

	rawItem := l.items[idx]
	entry := l.cache[rawItem]

	// Run render callbacks. Callbacks may mutate the item (focus,
	// highlight) which in turn bumps its paint version when state
	// actually changes. We capture the post-callback paint version
	// below.
	item := rawItem
	if len(l.renderCallbacks) > 0 {
		for _, cb := range l.renderCallbacks {
			if it := cb(idx, l.selectedIdx, item); it != nil {
				item = it
			}
		}
	}

	paintV := rawItem.PaintVersion()
	if entry != nil && entry.width == l.width && entry.paintV == paintV {
		// Cache hit — frozen or unfrozen, the entry content is
		// still correct because no paint bump landed since the
		// last render. Selection-drag suppression turns this into
		// a miss only if the entry is frozen.
		if !entry.frozen {
			return entry
		}
		if _, suppressed := l.freezeSuppressed[rawItem]; !suppressed {
			return entry
		}
	}

	rendered := item.Render(l.width)
	rendered = strings.TrimRight(rendered, "\n")
	lines := strings.Split(rendered, "\n")
	height := len(lines)

	// Re-read both versions after Render so that any bumps caused by
	// Render itself (e.g. an item that mutates internal state during
	// rendering) are captured. Without this we would freeze a stale
	// entry under the post-render versions.
	finalPaintV := rawItem.PaintVersion()
	finalLayoutV := rawItem.LayoutVersion()

	frozen := false
	if rawItem.Finished() {
		if _, suppressed := l.freezeSuppressed[rawItem]; !suppressed {
			frozen = true
		}
	}

	if entry == nil {
		entry = &listCacheEntry{}
		l.cache[rawItem] = entry
	}
	// If the item's rendered height changed, the cached approximate
	// total height is no longer valid and must be recomputed on the
	// next TotalHeightApprox call.
	if entry.height != height {
		l.totalHeightValid = false
	}
	entry.width = l.width
	entry.paintV = finalPaintV
	entry.layoutV = finalLayoutV
	entry.frozen = frozen
	entry.content = rendered
	entry.lines = lines
	entry.height = height
	return entry
}

// invalidateAll drops every cache entry. Called on width changes.
func (l *List) invalidateAll() {
	for k := range l.cache {
		delete(l.cache, k)
	}
	l.totalHeightValid = false
}

// Invalidate drops the cache entry for the given item, forcing a
// re-render on the next getItem call. No-op if the item is not in
// the cache.
func (l *List) Invalidate(item Item) {
	delete(l.cache, item)
}

// InvalidateFrozen drops the frozen flag (and stored content) for the
// given item. Equivalent to Invalidate but exposed under the F6
// frozen-items vocabulary so external callers can express intent.
func (l *List) InvalidateFrozen(item Item) {
	delete(l.cache, item)
}

// retainCacheFor drops every cache entry whose key is not in the given
// item set. Used by SetItems to keep entries for stable items while
// dropping entries for removed ones.
func (l *List) retainCacheFor(items []Item) {
	if len(l.cache) == 0 {
		return
	}
	keep := make(map[Item]struct{}, len(items))
	for _, it := range items {
		keep[it] = struct{}{}
	}
	for k := range l.cache {
		if _, ok := keep[k]; !ok {
			delete(l.cache, k)
		}
	}
}

// BeginSelectionDrag marks the items in the inclusive [startIdx, endIdx]
// range as un-freezable for the duration of an active selection drag.
// Frozen entries inside the range are dropped so the next render
// reflects live selection-overlay output. The corresponding
// EndSelectionDrag clears the suppression set and lets items
// re-freeze on their next render. Indices outside the items slice
// are clipped silently.
func (l *List) BeginSelectionDrag(startIdx, endIdx int) {
	if len(l.items) == 0 {
		return
	}
	if startIdx > endIdx {
		startIdx, endIdx = endIdx, startIdx
	}
	startIdx = max(startIdx, 0)
	endIdx = min(endIdx, len(l.items)-1)
	for i := startIdx; i <= endIdx; i++ {
		it := l.items[i]
		l.freezeSuppressed[it] = struct{}{}
		// Drop any cached frozen entry so the next render rebuilds
		// it as a live (un-frozen) entry that picks up the
		// selection overlay.
		if entry, ok := l.cache[it]; ok && entry.frozen {
			delete(l.cache, it)
		}
	}
}

// EndSelectionDrag clears the selection-drag freeze suppression. Items
// inside the previous range will re-freeze on their next render once
// their Finished() reports true again.
func (l *List) EndSelectionDrag() {
	for k := range l.freezeSuppressed {
		delete(l.freezeSuppressed, k)
		// Drop the cache entry so the next render produces a clean
		// (un-highlighted) frozen entry.
		delete(l.cache, k)
	}
}

// ScrollToIndex scrolls the list to the given item index. It detaches
// from bottom-anchored follow: the user asked for a specific item, so
// the viewport should stop tracking the end. If the resulting position
// happens to be at the true bottom, AtBottom() will still report so
// from geometry, and a subsequent downward ScrollBy will re-anchor.
func (l *List) ScrollToIndex(index int) {
	if index < 0 {
		index = 0
	}
	if index >= len(l.items) {
		index = len(l.items) - 1
	}
	l.bottomAnchored = false
	l.offsetIdx = index
	l.offsetLine = 0
}

// ScrollBy scrolls the list by the given number of lines.
//
// Direction interacts with follow mode. Scrolling down while anchored
// is a no-op (AtBottom is already true). Scrolling up first
// materializes the anchor into concrete offsets, then detaches, so the
// walk moves away from a real position rather than a derived one.
// Scrolling down that reaches the true bottom re-anchors via the clamp
// instead of leaving the viewport a few lines short.
func (l *List) ScrollBy(lines int) {
	if len(l.items) == 0 || lines == 0 {
		return
	}

	if l.reverse {
		lines = -lines
	}

	if lines > 0 {
		if l.AtBottom() {
			// Already at bottom (definitionally true while
			// anchored, or by exact geometry otherwise).
			return
		}

		// Scroll down
		l.offsetLine += lines
		currentItem := l.getItem(l.offsetIdx)
		for l.offsetLine >= currentItem.height {
			l.offsetLine -= currentItem.height
			if l.gap > 0 {
				l.offsetLine = max(0, l.offsetLine-l.gap)
			}

			// Move to next item
			l.offsetIdx++
			if l.offsetIdx > len(l.items)-1 {
				// Reached bottom
				l.ScrollToBottom()
				return
			}
			currentItem = l.getItem(l.offsetIdx)
		}

		lastOffsetIdx, lastOffsetLine, _ := l.lastOffsetItem()
		if l.offsetIdx > lastOffsetIdx || (l.offsetIdx == lastOffsetIdx && l.offsetLine > lastOffsetLine) {
			// Reached or passed the true (exact-height) bottom.
			// Clamp to it and re-anchor so that subsequent content
			// growth keeps the viewport glued to the end rather
			// than fighting the user back up from a stale estimate.
			l.offsetIdx = lastOffsetIdx
			l.offsetLine = lastOffsetLine
			l.bottomAnchored = true
		}
	} else if lines < 0 {
		// Scrolling up detaches from follow. Materialize the derived
		// offsets from the anchor first so the walk starts at the
		// real bottom position, then clear the anchor.
		l.syncOffsetsIfAnchored()
		l.bottomAnchored = false

		// Scroll up
		l.offsetLine += lines // lines is negative
		for l.offsetLine < 0 {
			// Move to previous item
			l.offsetIdx--
			if l.offsetIdx < 0 {
				// Reached top
				l.ScrollToTop()
				break
			}
			prevItem := l.getItem(l.offsetIdx)
			totalHeight := prevItem.height
			if l.gap > 0 {
				totalHeight += l.gap
			}
			l.offsetLine += totalHeight
		}
	}
}

// syncOffsetsIfAnchored materializes the derived scroll offsets from
// the exact-height bottom when the list is bottom-anchored. It leaves
// bottomAnchored untouched so callers can choose whether to keep
// following; it exists so consumers that read offsetIdx/offsetLine
// (Render, ScrollBy up) start from the real end position rather than
// stale authoritative offsets left behind by an earlier detach.
func (l *List) syncOffsetsIfAnchored() {
	if !l.bottomAnchored || len(l.items) == 0 {
		return
	}
	lastOffsetIdx, lastOffsetLine, _ := l.lastOffsetItem()
	l.offsetIdx = lastOffsetIdx
	l.offsetLine = lastOffsetLine
}

// VisibleItemIndices finds the range of items that are visible in the
// viewport. This is used for checking if selected item is in view.
//
// It reads offsetIdx/offsetLine, so it first materializes them from
// the anchor when following. Heights come from heightExact: the range
// that decides whether a selected item is in view is a positioning
// question and must not be answered from stale heights. The walk is
// O(viewport) — it stops after one screenful — and this method runs on
// every animation tick, which is exactly why heightExact's layout-
// version fast path (no render for a paint-only spinner bump) matters
// here.
func (l *List) VisibleItemIndices() (startIdx, endIdx int) {
	if len(l.items) == 0 {
		return 0, 0
	}
	l.syncOffsetsIfAnchored()

	startIdx = l.offsetIdx
	currentIdx := startIdx
	visibleHeight := -l.offsetLine

	for currentIdx < len(l.items) {
		itemHeight := l.heightExact(currentIdx)
		visibleHeight += itemHeight
		if l.gap > 0 {
			visibleHeight += l.gap
		}

		if visibleHeight >= l.height {
			break
		}
		currentIdx++
	}

	endIdx = currentIdx
	if endIdx >= len(l.items) {
		endIdx = len(l.items) - 1
	}

	return startIdx, endIdx
}

// Render renders the list and returns the visible lines.
//
// F7: per-item slicing is bounded by the remaining viewport budget so
// per-frame work is O(viewport) rather than O(total item heights).
// We never append beyond l.height lines to the output buffer; the
// final trim is therefore unnecessary. Reverse mode applies the same
// final reversal as before, which is byte-identical because the
// pre-F7 trim happened at the tail of the joined buffer (the same
// lines we now drop implicitly per item).
func (l *List) Render() string {
	if len(l.items) == 0 {
		return ""
	}

	// While following, derive the concrete offsets from the exact
	// bottom before rendering. This is the crux of the follow design:
	// the anchor IS the bottom, recomputed here with exact heights, so
	// appends and layout-changing growth since the last frame simply
	// re-hang content upward from the last line — no stale offset can
	// leave the viewport short of the end.
	l.syncOffsetsIfAnchored()

	budget := max(l.height, 0)
	lines := make([]string, 0, budget)
	currentIdx := l.offsetIdx
	currentOffset := l.offsetLine

	for currentIdx < len(l.items) {
		remaining := budget - len(lines)
		if remaining <= 0 {
			break
		}

		entry := l.renderItemEntry(currentIdx)
		if entry == nil {
			break
		}
		itemLines := entry.lines
		itemHeight := len(itemLines)

		if currentOffset >= 0 && currentOffset < itemHeight {
			// Append only the visible slice that fits in the
			// remaining viewport budget. Anything past the
			// budget would be discarded by the pre-F7 tail
			// trim, so skipping the append here is
			// byte-identical and bounded.
			visible := itemLines[currentOffset:]
			if len(visible) > remaining {
				visible = visible[:remaining]
			}
			lines = append(lines, visible...)

			// Gap rows after the item, capped to the
			// remaining budget so a 30k-line item with a
			// trailing gap can't push past the viewport.
			if l.gap > 0 {
				gapBudget := min(budget-len(lines), l.gap)
				for range gapBudget {
					lines = append(lines, "")
				}
			}
		} else {
			// offsetLine starts inside the gap.
			gapOffset := currentOffset - itemHeight
			gapRemaining := l.gap - gapOffset
			if gapRemaining > 0 {
				gapBudget := min(budget-len(lines), gapRemaining)
				for range gapBudget {
					lines = append(lines, "")
				}
			}
		}

		currentIdx++
		currentOffset = 0 // Reset offset for subsequent items.
	}

	l.height = budget

	if l.reverse {
		// Reverse the lines so the list renders bottom-to-top.
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
	}

	return strings.Join(lines, "\n")
}

// PrependItems prepends items to the list.
func (l *List) PrependItems(items ...Item) {
	l.items = append(items, l.items...)

	// Keep view position relative to the content that was visible
	l.offsetIdx += len(items)

	// Update selection index if valid
	if l.selectedIdx != -1 {
		l.selectedIdx += len(items)
	}
	l.totalHeightValid = false
}

// SetItems sets the items in the list. Cache entries for items that
// remain after the swap are preserved; entries for removed items are
// dropped.
//
// The offsetIdx clamp and offsetLine reset matter only when NOT
// following: they keep a detached scroll position in range after the
// item set shrinks. While bottomAnchored, the offsets are derived and
// these writes are overwritten by the next Render, so no anchor-aware
// bookkeeping is needed here.
func (l *List) SetItems(items ...Item) {
	l.items = items
	l.selectedIdx = min(l.selectedIdx, len(l.items)-1)
	l.offsetIdx = min(l.offsetIdx, len(l.items)-1)
	l.offsetLine = 0
	l.retainCacheFor(items)
	l.totalHeightValid = false
}

// AppendItems appends items to the list. No offset bookkeeping is
// needed for either mode: while following, the next Render re-derives
// the anchor and keeps the viewport glued to the new end; while
// detached, existing offsets still point at the same (unshifted)
// items.
func (l *List) AppendItems(items ...Item) {
	l.items = append(l.items, items...)
	l.totalHeightValid = false
}

// RemoveItem removes the item at the given index from the list.
func (l *List) RemoveItem(idx int) {
	if idx < 0 || idx >= len(l.items) {
		return
	}

	removed := l.items[idx]

	// Remove the item
	l.items = append(l.items[:idx], l.items[idx+1:]...)

	// Drop the cache entry for the removed item; entries for stable
	// items stay valid because they are keyed by pointer, not index.
	delete(l.cache, removed)
	delete(l.freezeSuppressed, removed)

	// Adjust selection if needed
	if l.selectedIdx == idx {
		l.selectedIdx = -1
	} else if l.selectedIdx > idx {
		l.selectedIdx--
	}

	// Adjust offset if needed
	if l.offsetIdx > idx {
		l.offsetIdx--
	} else if l.offsetIdx == idx && l.offsetIdx >= len(l.items) {
		l.offsetIdx = max(0, len(l.items)-1)
		l.offsetLine = 0
	}
	l.totalHeightValid = false
}

// Focused returns whether the list is focused.
func (l *List) Focused() bool {
	return l.focused
}

// Focus sets the focus state of the list.
func (l *List) Focus() {
	l.focused = true
}

// Blur removes the focus state from the list.
func (l *List) Blur() {
	l.focused = false
}

// ScrollToTop scrolls the list to the top. It detaches from
// bottom-anchored follow.
func (l *List) ScrollToTop() {
	l.bottomAnchored = false
	l.offsetIdx = 0
	l.offsetLine = 0
}

// ScrollToBottom scrolls the list to the bottom and engages
// bottom-anchored follow: from now until an explicit detach the
// viewport stays glued to the end, so appends and height growth keep
// the last line visible without any re-scroll. The offsets are also
// synced eagerly here so readers that consume them before the next
// Render (e.g. AtBottom's non-anchored path is bypassed, but other
// callers may read offsetIdx) see the true bottom immediately.
func (l *List) ScrollToBottom() {
	if len(l.items) == 0 {
		return
	}

	l.bottomAnchored = true
	lastOffsetIdx, lastOffsetLine, _ := l.lastOffsetItem()
	l.offsetIdx = lastOffsetIdx
	l.offsetLine = lastOffsetLine
}

// ScrollToSelected scrolls the list to the selected item. It detaches
// from follow because the user is navigating to a specific item;
// heights come from heightExact so the placement is exact.
func (l *List) ScrollToSelected() {
	if l.selectedIdx < 0 || l.selectedIdx >= len(l.items) {
		return
	}

	startIdx, endIdx := l.VisibleItemIndices()
	if l.selectedIdx < startIdx {
		// Selected item is above the visible range
		l.bottomAnchored = false
		l.offsetIdx = l.selectedIdx
		l.offsetLine = 0
	} else if l.selectedIdx > endIdx {
		// Selected item is below the visible range
		// Scroll so that the selected item is at the bottom
		l.bottomAnchored = false
		var totalHeight int
		for i := l.selectedIdx; i >= 0; i-- {
			totalHeight += l.heightExact(i)
			if l.gap > 0 && i < l.selectedIdx {
				totalHeight += l.gap
			}
			if totalHeight >= l.height {
				l.offsetIdx = i
				l.offsetLine = totalHeight - l.height
				break
			}
		}
		if totalHeight < l.height {
			// All items fit in the viewport
			l.ScrollToTop()
		}
	}
}

// SelectedItemInView returns whether the selected item is currently in view.
func (l *List) SelectedItemInView() bool {
	if l.selectedIdx < 0 || l.selectedIdx >= len(l.items) {
		return false
	}
	startIdx, endIdx := l.VisibleItemIndices()
	return l.selectedIdx >= startIdx && l.selectedIdx <= endIdx
}

// SetSelected sets the selected item index in the list.
// It returns -1 if the index is out of bounds.
func (l *List) SetSelected(index int) {
	if index < 0 || index >= len(l.items) {
		l.selectedIdx = -1
	} else {
		l.selectedIdx = index
	}
}

// Selected returns the index of the currently selected item. It returns -1 if
// no item is selected.
func (l *List) Selected() int {
	return l.selectedIdx
}

// IsSelectedFirst returns whether the first item is selected.
func (l *List) IsSelectedFirst() bool {
	return l.selectedIdx == 0
}

// IsSelectedLast returns whether the last item is selected.
func (l *List) IsSelectedLast() bool {
	return l.selectedIdx == len(l.items)-1
}

// SelectPrev selects the visually previous item (moves toward visual top).
// It returns whether the selection changed.
func (l *List) SelectPrev() bool {
	if l.reverse {
		// In reverse, visual up = higher index
		if l.selectedIdx < len(l.items)-1 {
			l.selectedIdx++
			return true
		}
	} else {
		// Normal: visual up = lower index
		if l.selectedIdx > 0 {
			l.selectedIdx--
			return true
		}
	}
	return false
}

// SelectNext selects the next item in the list.
// It returns whether the selection changed.
func (l *List) SelectNext() bool {
	if l.reverse {
		// In reverse, visual down = lower index
		if l.selectedIdx > 0 {
			l.selectedIdx--
			return true
		}
	} else {
		// Normal: visual down = higher index
		if l.selectedIdx < len(l.items)-1 {
			l.selectedIdx++
			return true
		}
	}
	return false
}

// SelectFirst selects the first item in the list.
// It returns whether the selection changed.
func (l *List) SelectFirst() bool {
	if len(l.items) == 0 {
		return false
	}
	l.selectedIdx = 0
	return true
}

// SelectLast selects the last item in the list (highest index).
// It returns whether the selection changed.
func (l *List) SelectLast() bool {
	if len(l.items) == 0 {
		return false
	}
	l.selectedIdx = len(l.items) - 1
	return true
}

// WrapToStart wraps selection to the visual start (for circular navigation).
// In normal mode, this is index 0. In reverse mode, this is the highest index.
func (l *List) WrapToStart() bool {
	if len(l.items) == 0 {
		return false
	}
	if l.reverse {
		l.selectedIdx = len(l.items) - 1
	} else {
		l.selectedIdx = 0
	}
	return true
}

// WrapToEnd wraps selection to the visual end (for circular navigation).
// In normal mode, this is the highest index. In reverse mode, this is index 0.
func (l *List) WrapToEnd() bool {
	if len(l.items) == 0 {
		return false
	}
	if l.reverse {
		l.selectedIdx = 0
	} else {
		l.selectedIdx = len(l.items) - 1
	}
	return true
}

// SelectedItem returns the currently selected item. It may be nil if no item
// is selected.
func (l *List) SelectedItem() Item {
	if l.selectedIdx < 0 || l.selectedIdx >= len(l.items) {
		return nil
	}
	return l.items[l.selectedIdx]
}

// SelectFirstInView selects the first item currently in view.
func (l *List) SelectFirstInView() {
	startIdx, _ := l.VisibleItemIndices()
	l.selectedIdx = startIdx
}

// SelectLastInView selects the last item currently in view.
func (l *List) SelectLastInView() {
	_, endIdx := l.VisibleItemIndices()
	l.selectedIdx = endIdx
}

// ItemAt returns the item at the given index.
func (l *List) ItemAt(index int) Item {
	if index < 0 || index >= len(l.items) {
		return nil
	}
	return l.items[index]
}

// ItemIndexAtPosition returns the item at the given viewport-relative y
// coordinate. Returns the item index and the y offset within that item. It
// returns -1, -1 if no item is found.
func (l *List) ItemIndexAtPosition(x, y int) (itemIdx int, itemY int) {
	return l.findItemAtY(x, y)
}

// findItemAtY finds the item at the given viewport y coordinate.
// Returns the item index and the y offset within that item. It returns -1, -1
// if no item is found.
func (l *List) findItemAtY(_, y int) (itemIdx int, itemY int) {
	if y < 0 || y >= l.height {
		return -1, -1
	}
	// Mouse hit-testing reads the concrete offsets; make sure they
	// reflect the derived bottom when following.
	l.syncOffsetsIfAnchored()

	// Walk through visible items to find which one contains this y
	currentIdx := l.offsetIdx
	currentLine := -l.offsetLine // Negative because offsetLine is how many lines are hidden

	for currentIdx < len(l.items) && currentLine < l.height {
		item := l.getItem(currentIdx)
		itemEndLine := currentLine + item.height

		// Check if y is within this item's visible range
		if y >= currentLine && y < itemEndLine {
			// Found the item, calculate itemY (offset within the item)
			itemY = y - currentLine
			return currentIdx, itemY
		}

		// Move to next item
		currentLine = itemEndLine
		if l.gap > 0 {
			currentLine += l.gap
		}
		currentIdx++
	}

	return -1, -1
}
