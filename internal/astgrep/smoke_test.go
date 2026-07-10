package astgrep

import (
	"fmt"
	"testing"
)

func TestSmoke(t *testing.T) {
	cases := []struct {
		name, path, src, pattern string
		wantN                    int
		wantEnclosing            string // enclosing of first match, "" to skip check
		wantBind                 map[string]string
	}{
		{
			name:    "ts_logical_or",
			path:    "a.ts",
			src:     "function f(x: number) {\n  return x > 0 || x < -1;\n}\nconst y = a || b;\n",
			pattern: "$A || $B",
			wantN:   2,
		},
		{
			name:     "ts_call_ellipsis",
			path:     "a.ts",
			src:      "foo(a, b, c);\nbar();\nfoo(1);\n",
			pattern:  "foo($$$ARGS)",
			wantN:    2,
			wantBind: map[string]string{"ARGS": "a, b, c"},
		},
		{
			name:          "go_binary",
			path:          "a.go",
			src:           "package m\n\nfunc add(a int, b int) int {\n\treturn a + b\n}\n",
			pattern:       "$A + $B",
			wantN:         1,
			wantEnclosing: "add",
		},
		{
			name:    "py_compare",
			path:    "a.py",
			src:     "def f(x):\n    if x > 0:\n        return x\n    return 0\n",
			pattern: "$X > 0",
			wantN:   1,
		},
		{
			name:    "ts_const_arrow",
			path:    "a.ts",
			src:     "const add = (a, b) => a + b;\nconst z = 3;\n",
			pattern: "const $NAME = ($$$ARGS) => $BODY",
			wantN:   1,
			wantBind: map[string]string{
				"NAME": "add", "ARGS": "a, b", "BODY": "a + b",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ms, err := Search(c.pattern, c.src, c.path)
			if err != nil {
				t.Fatalf("Search error: %v", err)
			}
			if len(ms) != c.wantN {
				t.Fatalf("got %d matches, want %d: %s", len(ms), c.wantN, dump(ms))
			}
			if c.wantEnclosing != "" && ms[0].Enclosing != c.wantEnclosing {
				t.Errorf("enclosing = %q, want %q", ms[0].Enclosing, c.wantEnclosing)
			}
			for k, v := range c.wantBind {
				if got := ms[0].Bindings[k]; got != v {
					t.Errorf("binding %s = %q, want %q", k, got, v)
				}
			}
		})
	}
}

func dump(ms []Match) string {
	s := ""
	for _, m := range ms {
		s += fmt.Sprintf("\n  L%d %q %v", m.StartLine, m.Text, m.Bindings)
	}
	return s
}
