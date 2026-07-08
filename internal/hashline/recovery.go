package hashline

import (
	udiff "github.com/aymanbagabas/go-udiff"
)

// Recover attempts to apply edits (anchored against base) onto a drifted live
// file via a 3-way merge.
//
// base is the file version the edits were anchored to — its tag matched the
// section header, so line numbers are valid against it. live is the current
// on-disk content, which has diverged from base since it was read. Recovery
// applies the intended edit to base, then merges that change with the drift
// (base -> live) using go-udiff's edit merge.
//
// It returns the merged result and true only when the two changes do not
// overlap; on any conflict it returns false so the caller falls back to a
// clean stale-tag rejection. This makes recovery strictly safe: it never
// silently resolves a conflict.
func Recover(base, live string, edits []Edit) (string, bool) {
	applied, err := Apply(base, edits)
	if err != nil {
		return "", false
	}
	drift := udiff.Lines(base, live)
	intent := udiff.Lines(base, applied.Text)
	merged, ok := udiff.Merge(drift, intent)
	if !ok {
		return "", false
	}
	result, err := udiff.Apply(base, merged)
	if err != nil {
		return "", false
	}
	return result, true
}
