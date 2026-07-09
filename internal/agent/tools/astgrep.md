Search code by syntax-tree shape, not text. You write a pattern that looks like code with metavariable holes; it matches any code with the same structure, ignoring formatting, comments, and string contents.

Use this to locate the RIGHT occurrence of a repeated construct — when a bug involves one of several similar expressions (e.g. one `||` that should be `??`, one of four identical lines), plain text search returns noise and misses multi-line forms. AstGrep returns each structural match with its line, column, and enclosing function/class, so you can pick the correct site before editing.

## Pattern syntax
- `$NAME` — captures exactly one node (uppercase/underscore-led name). Example: `$X.trim()`.
- `$$$NAME` — captures a sequence of sibling nodes, e.g. all call arguments or statements. Example: `foo($$$ARGS)`.
- `$_` — matches one node without capturing (wildcard).
- A repeated metavariable must match identical text: `$X === $X` matches `a === a` but not `a === b`.
- Literal tokens are exact: `foo(1)` matches `foo(1)` but not `foo(2)`.

The pattern is parsed with the target file's grammar, so it must be a valid code fragment in that language (an expression, statement, or declaration).

## Parameters
- `pattern` (required): the structural pattern.
- `path` (optional): a file or directory. Defaults to the working directory. The language is inferred from each file's extension; the pattern only matches files of the language it parses as.

## Output
Matches grouped by file:

```
src/render/rubygems.ts
  89:24  (in handleRubyGems)  const runtimeDeps = gem.dependencies.runtime;
  98:22  (in handleRubyGems)  const devDeps = gem.dependencies?.development;
```

with a `meta:` line listing metavariable bindings when the pattern has captures. Results are capped; narrow the pattern or path if truncated.

## Examples
- `$A || $B` — every logical-or expression (won't match `??`, `&&`, or `||` in a string).
- `$X == $Y` — loose-equality comparisons (find `==` that should be `===`).
- `foo($$$ARGS)` — all calls to `foo`, with their arguments captured.
- `if ($C) { return $X; }` — a specific guarded-return shape.

This tool is read-only; it does not modify files.
