package memory

import (
	"strings"
	"unicode"
)

// ftsStopwords are common words dropped from FTS queries to keep matches
// meaningful. Kept small on purpose.
var ftsStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true,
	"be": true, "to": true, "of": true, "in": true, "on": true, "for": true,
	"and": true, "or": true, "it": true, "this": true, "that": true, "with": true,
	"as": true, "at": true, "by": true, "from": true, "we": true, "you": true,
	"i": true, "do": true, "does": true, "how": true, "what": true, "when": true,
}

// tokenize splits text into lowercased word tokens (letters, digits, underscore).
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	return fields
}

// queryTokens returns the distinct, stopword-filtered tokens (>=2 chars) of a
// query, in first-seen order.
func queryTokens(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tokenize(s) {
		if len(t) < 2 || ftsStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// ftsMatchQuery builds a safe FTS5 MATCH expression from free text: each token
// is quoted (so punctuation can never form invalid FTS syntax) and OR-joined.
// It returns "" when the query has no usable tokens.
func ftsMatchQuery(s string) string {
	toks := queryTokens(s)
	if len(toks) == 0 {
		return ""
	}
	if len(toks) > 16 {
		toks = toks[:16]
	}
	quoted := make([]string, len(toks))
	for i, t := range toks {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}
	return strings.Join(quoted, " OR ")
}

// tokenSet returns the distinct token set of text (no stopword filtering — used
// for duplicate similarity).
func tokenSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, t := range tokenize(s) {
		set[t] = true
	}
	return set
}

// jaccard is the token-set Jaccard similarity of two strings, in [0, 1].
func jaccard(a, b string) float64 {
	sa, sb := tokenSet(a), tokenSet(b)
	if len(sa) == 0 && len(sb) == 0 {
		return 1
	}
	inter := 0
	for t := range sa {
		if sb[t] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
