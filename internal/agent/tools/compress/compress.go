// Package compress shrinks verbose, LLM-style prose (the kind commonly
// found in MCP tool, prompt, and resource descriptions) down to its
// essential content, to reduce the number of tokens spent describing a
// tool to the model. It strips filler words, pleasantries, hedging
// phrases, sentence-leading throat-clearing ("I'll", "let's", ...), and
// redundant articles, while never touching code, URLs, paths, or other
// content that must survive verbatim.
package compress

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Sentinel delimiters used to protect spans of text that must survive
// compression untouched. \x00 and \x01 are ASCII control characters that
// never appear in ordinary prose and are not RE2 "word" characters, so
// \b boundaries around a sentinel behave the same as around any other
// punctuation.
const (
	sentinelOpen  = "\x00"
	sentinelClose = "\x01"
)

var sentinelRe = regexp.MustCompile(sentinelOpen + `([0-9]+)` + sentinelClose)

// protector records spans of text that must not be rewritten by the
// prose transforms below, replacing each with a sentinel placeholder
// that the transforms will pass through unchanged, and later restoring
// the original text in place of each placeholder.
type protector struct {
	items []string
}

// store records s and returns the sentinel placeholder standing in for it.
func (p *protector) store(s string) string {
	idx := len(p.items)
	p.items = append(p.items, s)
	return sentinelOpen + strconv.Itoa(idx) + sentinelClose
}

// restore replaces every sentinel placeholder in s with the original text
// it stands in for. It loops until no placeholders remain, since one
// protected span (e.g. a function call) can itself contain another (e.g.
// a CONST_CASE argument) that was substituted first.
func (p *protector) restore(s string) string {
	for {
		next := sentinelRe.ReplaceAllStringFunc(s, func(m string) string {
			sub := sentinelRe.FindStringSubmatch(m)
			idx, err := strconv.Atoi(sub[1])
			if err != nil || idx < 0 || idx >= len(p.items) {
				return m
			}
			return p.items[idx]
		})
		if next == s {
			return next
		}
		s = next
	}
}

// Regexes for spans of text that must never be rewritten. Each one starts
// its match on a "word" character (a letter or digit) except pathRe, which
// needs a manual boundary check instead of \b -- see protectPath.
var (
	fencedCodeBlockRe = regexp.MustCompile("(?s)```.*?```")
	inlineCodeRe      = regexp.MustCompile("`[^`\n]+`")
	urlRe             = regexp.MustCompile(`\b(?:https?|ftp)://[^\s)\]>"'` + "`" + `]+`)
	semverRe          = regexp.MustCompile(`\bv?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?\b`)
	constCaseRe       = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\b`)
	dottedCallRe      = regexp.MustCompile(`\b[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)+\([^()]*\)`)
	functionCallRe    = regexp.MustCompile(`\b[A-Za-z_]\w*\([^()]*\)`)

	// pathRe matches filesystem paths (./foo/bar, ../baz, ~/qux,
	// /etc/passwd, C:\Users\foo). Unlike the regexes above, a path can
	// start with a non-word character (/, ., ~), so a plain \b before it
	// does not reliably reject a match in the middle of an unrelated
	// word (e.g. the "/or" in "and/or"). Go's regexp package (RE2) has no
	// lookbehind to express "not preceded by a word character" without
	// consuming it, so group 1 below consumes the boundary explicitly
	// (start of string, or a single non-word character) and the
	// replacement puts it back unchanged; only group 2, the path itself,
	// is protected. See protectPath.
	pathRe = regexp.MustCompile(`(^|[^\w])((?:~|\.{1,2})?/(?:[\w.-]+/)*[\w.-]+|[A-Za-z]:\\[\w.\\-]+)`)
)

// protectPath protects filesystem-path matches in text, preserving the
// leading boundary character (or start-of-string) that pathRe had to
// consume to rule out matching mid-identifier.
func protectPath(text string, p *protector) string {
	return pathRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := pathRe.FindStringSubmatch(m)
		return sub[1] + p.store(sub[2])
	})
}

// protectAll replaces every protected span in text with a sentinel
// placeholder and returns the resulting text. Order matters: broader
// spans (fenced code blocks, then inline code, then URLs) are protected
// first so that narrower patterns (paths, CONST_CASE, calls) never fire
// on text that is already spoken for.
func (p *protector) protectAll(text string) string {
	text = fencedCodeBlockRe.ReplaceAllStringFunc(text, p.store)
	text = inlineCodeRe.ReplaceAllStringFunc(text, p.store)
	text = urlRe.ReplaceAllStringFunc(text, p.store)
	text = protectPath(text, p)
	text = semverRe.ReplaceAllStringFunc(text, p.store)
	text = constCaseRe.ReplaceAllStringFunc(text, p.store)
	text = dottedCallRe.ReplaceAllStringFunc(text, p.store)
	text = functionCallRe.ReplaceAllStringFunc(text, p.store)
	return text
}

// Filler words: intensifiers/qualifiers that add no information.
var fillerWordRe = regexp.MustCompile(`(?i)\b(?:just|really|basically|actually|simply|quite|very|essentially|literally)\b`)

// Pleasantries: politeness that a tool description does not need. The
// trailing character class also eats the punctuation and whitespace that
// usually rides along with these phrases (e.g. the comma in "Please,
// fix it"), per the requirement that pleasantries take their trailing
// punctuation with them.
var pleasantryRe = regexp.MustCompile(`(?i)\b(?:thank you|i'd be happy|of course|happy to|please|kindly|thanks|sure|certainly)\b[\s,.!]*`)

// Hedging phrases: verbal hedges that weaken a statement without adding
// meaning.
var hedgingRe = regexp.MustCompile(`(?i)\b(?:could potentially|would like to|in my opinion|it seems|it appears|perhaps|maybe|might|i think)\b`)

// Sentence-leading throat-clearing, matched only at the start of a line
// (not mid-sentence, where "I can" or "we will" may be load-bearing).
// Like pleasantryRe, it also eats trailing punctuation/whitespace.
var sentenceLeadRe = regexp.MustCompile(`(?im)^(?:I'll|I will|I can|I'd|you can|we will|we can|let me|let's)\b[\s,.!]*`)

// Articles, stripped only when followed by whitespace and a lowercase
// word -- so "The API" (capitalized, likely a proper noun/acronym) is
// left alone, but "the file" is not. The reference implementation this
// is inspired by expressed that condition as a lookahead
// ((?=\s+[a-z])) so the article's surrounding whitespace was left
// untouched; RE2 (Go's regexp engine) has no lookahead, so instead the
// whole "article + whitespace + word" span is matched and replaced with
// just the word, which has the same net effect without requiring
// zero-width lookahead.
// Note the (?i) flag is scoped to just the article alternation via
// (?i:...), not the whole pattern -- otherwise it would also make the
// following [a-z] match uppercase letters, defeating the "The API"
// check above.
var articleRe = regexp.MustCompile(`\b(?i:a|an|the)\b\s+([a-z][\w'-]*)`)

// cleanupWhitespace collapses the whitespace disruption left behind by
// the strips above: runs of spaces/tabs, space before punctuation, and
// runs of 3+ newlines.
var (
	multiSpaceRe       = regexp.MustCompile(`[ \t]{2,}`)
	spaceBeforePunctRe = regexp.MustCompile(`[ \t]+([,.!?;:])`)
	extraNewlinesRe    = regexp.MustCompile(`\n{3,}`)
)

func cleanupWhitespace(s string) string {
	s = multiSpaceRe.ReplaceAllString(s, " ")
	s = spaceBeforePunctRe.ReplaceAllString(s, "$1")
	s = extraNewlinesRe.ReplaceAllString(s, "\n\n")
	return s
}

// capitalizeSentences uppercases the first letter of the string and the
// first letter following sentence-ending punctuation (. ! ?) or a
// newline, so that stripping a leading filler word or phrase does not
// leave a sentence starting with a lowercase letter. Sentinel
// placeholders contain no letters, so this pass cannot reach into (and
// therefore cannot corrupt) protected spans; it only ever touches real
// prose text.
func capitalizeSentences(s string) string {
	runes := []rune(s)
	capNext := true
	for i, r := range runes {
		switch {
		case r == '.' || r == '!' || r == '?' || r == '\n':
			capNext = true
		case unicode.IsSpace(r):
			// Whitespace does not end or start a sentence by itself;
			// leave capNext as-is.
		case unicode.IsLetter(r):
			if capNext {
				runes[i] = unicode.ToUpper(r)
			}
			capNext = false
		default:
			// Punctuation/digits/sentinel bytes other than the sentence
			// enders above: do not consume a pending capitalization
			// (e.g. an opening quote before the first real letter).
		}
	}
	return string(runes)
}

// CompressProse shrinks verbose prose text down to its essential
// content: filler words, pleasantries, hedging phrases, sentence-leading
// throat-clearing, and redundant articles are stripped, and the
// resulting whitespace is cleaned up. Fenced code blocks, inline code,
// URLs, filesystem paths, CONST_CASE identifiers, dotted method calls,
// generic function-call syntax, and semver version numbers are
// protected and always returned exactly as given.
func CompressProse(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}

	p := &protector{}
	s := p.protectAll(text)

	s = sentenceLeadRe.ReplaceAllString(s, "")
	s = pleasantryRe.ReplaceAllString(s, "")
	s = hedgingRe.ReplaceAllString(s, "")
	s = fillerWordRe.ReplaceAllString(s, "")
	s = articleRe.ReplaceAllString(s, "$1")

	s = cleanupWhitespace(s)
	s = capitalizeSentences(s)

	return strings.TrimSpace(p.restore(s))
}
