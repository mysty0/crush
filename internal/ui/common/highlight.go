package common

import (
	"bytes"
	"image/color"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	chromastyles "github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// lexerCache memoizes lexer lookups by file name. chroma's
// lexers.Match globs the name against every registered lexer's filename
// patterns (a filepath.Match per lexer), which is expensive and shows up
// as a dominant cost when rendering many code blocks (e.g. loading a long
// session). The result is deterministic per file name, so we cache it.
var (
	lexerCacheMu sync.RWMutex
	lexerCache   = map[string]chroma.Lexer{}
)

// lexerForFile returns a coalesced lexer for the given file name, using a
// cache to avoid repeated glob matching. Source is only consulted for
// content analysis on a cache miss when the name doesn't match.
func lexerForFile(fileName, source string) chroma.Lexer {
	// Key the cache by extension (or, when there's no extension, the base
	// name for special files like "Makefile"/"Dockerfile"). Many code
	// blocks in a session share a handful of extensions, so keying on the
	// extension turns thousands of expensive glob matches into a few.
	key := strings.ToLower(filepath.Ext(fileName))
	if key == "" {
		key = strings.ToLower(filepath.Base(fileName))
	}

	lexerCacheMu.RLock()
	l, ok := lexerCache[key]
	lexerCacheMu.RUnlock()
	if ok {
		return l
	}

	l = lexers.Match(fileName)
	if l == nil {
		l = lexers.Analyse(source)
	}
	if l == nil {
		l = lexers.Fallback
	}
	l = chroma.Coalesce(l)

	lexerCacheMu.Lock()
	lexerCache[key] = l
	lexerCacheMu.Unlock()
	return l
}

// SyntaxHighlight applies syntax highlighting to the given source code based
// on the file name and background color. It returns the highlighted code as a
// string.
func SyntaxHighlight(st *styles.Styles, source, fileName string, bg color.Color) (string, error) {
	// Determine the language lexer to use (cached by file name).
	l := lexerForFile(fileName, source)

	// Get the formatter
	f := formatters.Get("terminal16m")
	if f == nil {
		f = formatters.Fallback
	}

	style := chroma.MustNewStyle("crush", st.ChromaTheme())

	// Modify the style to use the provided background
	s, err := style.Builder().Transform(
		func(t chroma.StyleEntry) chroma.StyleEntry {
			r, g, b, _ := bg.RGBA()
			t.Background = chroma.NewColour(uint8(r>>8), uint8(g>>8), uint8(b>>8))
			return t
		},
	).Build()
	if err != nil {
		s = chromastyles.Fallback
	}

	// Tokenize and format
	it, err := l.Tokenise(nil, source)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	err = f.Format(&buf, s, it)
	return buf.String(), err
}
