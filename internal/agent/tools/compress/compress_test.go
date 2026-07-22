package compress

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompressProse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips filler words",
			in:   "This is just really a very simple test that is basically actually quite essentially literally trivial.",
			want: "This is simple test that is trivial.",
		},
		{
			name: "strips pleasantries with trailing punctuation",
			in:   "Please, could you kindly fix the bug? Thanks!",
			want: "Could you fix bug?",
		},
		{
			name: "strips hedging phrases",
			in:   "Perhaps this could potentially work.",
			want: "This work.",
		},
		{
			name: "strips sentence-leading phrases only at line start",
			in:   "I'll fix the bug now.\nLet's run the tests.\nWe can check the output.\nYou can verify it.",
			want: "Fix bug now.\nRun tests.\nCheck output.\nVerify it.",
		},
		{
			name: "does not strip sentence-leading phrase mid-sentence",
			in:   "This is what you can do next.",
			want: "This is what you can do next.",
		},
		{
			name: "strips articles a/an/the before a lowercase word",
			in:   "This is a quick fix for an easy bug.",
			want: "This is quick fix for easy bug.",
		},
		{
			name: "keeps article before a capitalized word",
			in:   "The API returns the response after the request.",
			want: "The API returns response after request.",
		},
		{
			name: "protects URLs",
			in:   "See https://example.com/docs/api?x=1&y=2 for the reference.",
			want: "See https://example.com/docs/api?x=1&y=2 for reference.",
		},
		{
			name: "protects inline code",
			in:   "Run `go test ./...` to check the tests.",
			want: "Run `go test ./...` to check tests.",
		},
		{
			name: "protects fenced code blocks",
			in:   "```go\nfunc main() {\n  fmt.Println(\"hi\")\n}\n```\nThat is the example.",
			want: "```go\nfunc main() {\n  fmt.Println(\"hi\")\n}\n```\nThat is example.",
		},
		{
			name: "protects filesystem paths",
			in:   "The path is /usr/local/bin/crush or ./internal/agent/tools/compress/compress.go, and also ~/config/crush.json.",
			want: "Path is /usr/local/bin/crush or ./internal/agent/tools/compress/compress.go, and also ~/config/crush.json.",
		},
		{
			name: "does not treat a mid-word slash as a path",
			in:   "and/or maybe this stays intact",
			want: "And/or this stays intact",
		},
		{
			name: "protects CONST_CASE identifiers",
			in:   "Set MAX_RETRIES and API_KEY to configure the client.",
			want: "Set MAX_RETRIES and API_KEY to configure client.",
		},
		{
			name: "protects dotted method calls and generic function calls",
			in:   "Call fmt.Println(\"hi\") or just run(foo, bar) to test it.",
			want: "Call fmt.Println(\"hi\") or run(foo, bar) to test it.",
		},
		{
			name: "protects semver version numbers",
			in:   "Upgrade to v1.2.3 or 2.0.0-beta.1 before continuing.",
			want: "Upgrade to v1.2.3 or 2.0.0-beta.1 before continuing.",
		},
		{
			name: "combines filler words, articles, and every protection",
			in:   "Please just really check the CONFIG_PATH and call obj.Method() at /etc/passwd for v1.0.0.",
			want: "Check the CONFIG_PATH and call obj.Method() at /etc/passwd for v1.0.0.",
		},
		{
			name: "returns whitespace-only input unchanged",
			in:   "   ",
			want: "   ",
		},
		{
			name: "returns empty input unchanged",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, CompressProse(tt.in))
		})
	}
}

// TestCompressProseIdempotent checks that running CompressProse a second
// time over its own output does not keep shrinking the text further,
// which would indicate the cleanup/capitalization pass is unstable.
func TestCompressProseIdempotent(t *testing.T) {
	t.Parallel()

	in := "Please just really check the CONFIG_PATH and call obj.Method() at /etc/passwd for v1.0.0."
	once := CompressProse(in)
	twice := CompressProse(once)
	require.Equal(t, once, twice)
}
