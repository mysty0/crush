package agent

import "testing"

func TestSanitizeSchemaName(t *testing.T) {
	t.Parallel()
	// Anthropic requires tool/schema names to match
	// ^[a-zA-Z0-9_-]{1,128}$. Workflow labels routinely contain
	// colons, spaces, and slashes, which must be replaced.
	tests := []struct {
		in   string
		want string
	}{
		{"search:Recent news", "search_Recent_news"},
		{"search:Official/authoritative", "search_Official_authoritative"},
		{"v0:claim text here", "v0_claim_text_here"},
		{"scope", "scope"},
		{"", "result"},
		{"!!!", "_"},
	}
	for _, tt := range tests {
		got := sanitizeSchemaName(tt.in)
		if got != tt.want {
			t.Errorf("sanitizeSchemaName(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if !schemaNameValid(got) {
			t.Errorf("sanitizeSchemaName(%q) = %q, which is not a valid schema name", tt.in, got)
		}
	}
}

// schemaNameValid reports whether name matches Anthropic's required
// pattern, used only to assert the sanitizer's output in tests.
func schemaNameValid(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
