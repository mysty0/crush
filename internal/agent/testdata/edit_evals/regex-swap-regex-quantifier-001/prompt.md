# Fix the bug in `twitter.ts`

A regex quantifier was swapped, changing whitespace matching.

The issue is on line 49.

Fix the ONE regex quantifier that was swapped (between `+` and `*`). Do not modify other quantifiers.