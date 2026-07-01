package model

import "testing"

func TestWordForwardSmall(t *testing.T) {
	lines := []string{"hello world"}
	tests := []struct {
		col      int
		wantLine int
		wantCol  int
	}{
		{col: 0, wantLine: 0, wantCol: 6},  // start of "hello" -> start of "world"
		{col: 3, wantLine: 0, wantCol: 6},  // mid "hello" -> start of "world"
		{col: 6, wantLine: 0, wantCol: 10}, // no more words: clamp to end of buffer
	}
	for _, tc := range tests {
		gotLine, gotCol := wordForward(lines, 0, tc.col, false)
		if gotLine != tc.wantLine || gotCol != tc.wantCol {
			t.Errorf("wordForward(col=%d) = (%d,%d), want (%d,%d)", tc.col, gotLine, gotCol, tc.wantLine, tc.wantCol)
		}
	}
}

func TestWordForwardPunctuation(t *testing.T) {
	// vim keyword classes split "foo.bar()" into: foo, ., bar, (, )
	lines := []string{"foo.bar()"}
	tests := []struct {
		col     int
		wantCol int
	}{
		{col: 0, wantCol: 3}, // "foo" -> "."
		{col: 3, wantCol: 4}, // "." -> "bar"
		{col: 4, wantCol: 7}, // "bar" -> "("
		{col: 7, wantCol: 8}, // "(" -> ")"
	}
	for _, tc := range tests {
		_, gotCol := wordForward(lines, 0, tc.col, false)
		if gotCol != tc.wantCol {
			t.Errorf("wordForward(col=%d) col = %d, want %d", tc.col, gotCol, tc.wantCol)
		}
	}
}

func TestWordForwardBigWord(t *testing.T) {
	lines := []string{"foo.bar() baz"}
	_, gotCol := wordForward(lines, 0, 0, true)
	if gotCol != 10 {
		t.Errorf("wordForward(big) col = %d, want 10", gotCol)
	}
}

func TestWordForwardAcrossLines(t *testing.T) {
	lines := []string{"hello", "world"}
	gotLine, gotCol := wordForward(lines, 0, 0, false)
	if gotLine != 1 || gotCol != 0 {
		t.Errorf("wordForward across lines = (%d,%d), want (1,0)", gotLine, gotCol)
	}
}

func TestWordForwardSkipsBlankLines(t *testing.T) {
	lines := []string{"hello", "", "world"}
	gotLine, gotCol := wordForward(lines, 0, 0, false)
	if gotLine != 2 || gotCol != 0 {
		t.Errorf("wordForward skip blank = (%d,%d), want (2,0)", gotLine, gotCol)
	}
}

func TestWordBackwardSmall(t *testing.T) {
	lines := []string{"hello world"}
	tests := []struct {
		col     int
		wantCol int
	}{
		{col: 6, wantCol: 0}, // start of "world" -> start of "hello"
		{col: 8, wantCol: 6}, // mid "world" -> start of "world"
		{col: 0, wantCol: 0}, // already at start: clamp
	}
	for _, tc := range tests {
		_, gotCol := wordBackward(lines, 0, tc.col, false)
		if gotCol != tc.wantCol {
			t.Errorf("wordBackward(col=%d) col = %d, want %d", tc.col, gotCol, tc.wantCol)
		}
	}
}

func TestWordBackwardAcrossLines(t *testing.T) {
	lines := []string{"hello", "world"}
	gotLine, gotCol := wordBackward(lines, 1, 0, false)
	if gotLine != 0 || gotCol != 0 {
		t.Errorf("wordBackward across lines = (%d,%d), want (0,0)", gotLine, gotCol)
	}
}

func TestWordEndSmall(t *testing.T) {
	lines := []string{"hello world"}
	tests := []struct {
		col     int
		wantCol int
	}{
		{col: 0, wantCol: 4},  // start of "hello" -> end of "hello"
		{col: 4, wantCol: 10}, // already at end of "hello" -> end of "world"
	}
	for _, tc := range tests {
		_, gotCol := wordEnd(lines, 0, tc.col, false)
		if gotCol != tc.wantCol {
			t.Errorf("wordEnd(col=%d) col = %d, want %d", tc.col, gotCol, tc.wantCol)
		}
	}
}

func TestWordEndAlwaysMovesForward(t *testing.T) {
	lines := []string{"hi"}
	// Cursor already at the last character; e must not return the same
	// position, it clamps to end-of-buffer (still the same char here
	// since there's nothing further, but the function must not loop).
	gotLine, gotCol := wordEnd(lines, 0, 1, false)
	if gotLine != 0 || gotCol != 1 {
		t.Errorf("wordEnd at buffer end = (%d,%d), want (0,1)", gotLine, gotCol)
	}
}

func TestLineFirstNonBlankCol(t *testing.T) {
	if got := lineFirstNonBlankCol("   hello"); got != 3 {
		t.Errorf("lineFirstNonBlankCol = %d, want 3", got)
	}
	if got := lineFirstNonBlankCol("   "); got != 0 {
		t.Errorf("lineFirstNonBlankCol(blank) = %d, want 0", got)
	}
	if got := lineFirstNonBlankCol(""); got != 0 {
		t.Errorf("lineFirstNonBlankCol(empty) = %d, want 0", got)
	}
}

func TestLineLastCol(t *testing.T) {
	if got := lineLastCol("hello"); got != 4 {
		t.Errorf("lineLastCol = %d, want 4", got)
	}
	if got := lineLastCol(""); got != 0 {
		t.Errorf("lineLastCol(empty) = %d, want 0", got)
	}
}

func TestEndOfBuffer(t *testing.T) {
	line, col := endOfBuffer([]string{"hello", "world"})
	if line != 1 || col != 4 {
		t.Errorf("endOfBuffer = (%d,%d), want (1,4)", line, col)
	}
	line, col = endOfBuffer(nil)
	if line != 0 || col != 0 {
		t.Errorf("endOfBuffer(nil) = (%d,%d), want (0,0)", line, col)
	}
}

func TestWideRuneWordSpans(t *testing.T) {
	// A wide (double-width) rune should occupy two display columns, so a
	// following ASCII word starts two columns after a single wide char.
	spans := smallWordSpans("我 world")
	if len(spans) != 2 {
		t.Fatalf("smallWordSpans(wide) = %v, want 2 spans", spans)
	}
	if spans[0].Start != 0 || spans[0].End != 2 {
		t.Errorf("wide rune span = %+v, want {0 2}", spans[0])
	}
	if spans[1].Start != 3 {
		t.Errorf("second span start = %d, want 3", spans[1].Start)
	}
}
