// Package hashline implements a compact, line-anchored patch language for
// LLM-driven file edits.
//
// A patch names lines to replace, delete, or insert at, then lists the new
// content. Every file section is bound to a 4-hex content hash of the whole
// normalized file, so a stale anchor (the file changed since it was read) is
// rejected before it can corrupt code.
//
// The package is pure: it performs no file I/O and does not depend on
// tree-sitter. Block operations are resolved through the injected
// [BlockResolver] interface, whose tree-sitter implementation lives in a
// separate package so this core stays lightweight and unit-testable.
package hashline

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// FileHashLength is the number of hex characters in a content-derived file
// hash tag.
const FileHashLength = 4

// Format sigils and keywords. These are the single source of truth for the
// parser, the formatter, and the prompt.
const (
	FilePrefix   = "["
	FileSuffix   = "]"
	FileHashSep  = "#"
	RangeSep     = ".="
	LineBodySep  = ":"
	PayloadSigil = "+"
	HeaderColon  = ":"

	KeywordSwap        = "SWAP"
	KeywordDelete      = "DEL"
	KeywordInsert      = "INS"
	KeywordSwapBlock   = "SWAP.BLK"
	KeywordDeleteBlock = "DEL.BLK"
	KeywordInsertBlock = "INS.BLK.POST"
	KeywordRemove      = "REM"
	KeywordMove        = "MV"

	InsertBefore = "PRE"
	InsertAfter  = "POST"
	InsertHead   = "HEAD"
	InsertTail   = "TAIL"
)

// normalizeForHash canonicalizes text before hashing so that CRLF endings and
// display-trimmed trailing whitespace do not invalidate a tag. It strips a
// leading UTF-8 BOM, converts CR/CRLF to LF, and trims trailing spaces and
// tabs from every line.
func normalizeForHash(text string) string {
	text = strings.TrimPrefix(text, "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

// ComputeFileHash returns the content-derived tag carried by a hashline
// section header: a 4-hex uppercase fingerprint of the whole file's normalized
// text. Any read of byte-identical content mints the same tag, and a follow-up
// edit anchored at any line validates whenever the live file still hashes to
// it.
//
// The hash is used only for consistency between the read producer and the edit
// consumer within a single Crush process; it is not compatible with any
// external format, so a stdlib hash suffices. Collisions on the 16-bit space
// are expected and handled by the snapshot store, which keys on full text.
func ComputeFileHash(text string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(normalizeForHash(text)))
	low16 := h.Sum32() & 0xffff
	return fmt.Sprintf("%0*X", FileHashLength, low16)
}

// FormatHeader formats a hashline section header for a file path and tag:
// "[path#TAG]".
func FormatHeader(path, tag string) string {
	return FilePrefix + path + FileHashSep + tag + FileSuffix
}

// FormatNumberedLine formats one displayed line as "LINE:TEXT".
func FormatNumberedLine(lineNumber int, line string) string {
	return fmt.Sprintf("%d%s%s", lineNumber, LineBodySep, line)
}

// FormatNumberedLines formats text with hashline-mode "LINE:TEXT" prefixes,
// numbering from startLine. Line endings are assumed already normalized to LF.
func FormatNumberedLines(text string, startLine int) string {
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(FormatNumberedLine(startLine+i, line))
	}
	return b.String()
}
