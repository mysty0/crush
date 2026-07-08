# Fix the bug in `artifacts.ts`

A regex quantifier was swapped, changing whitespace matching.

The issue is around the middle of the file.

Fix the ONE regex quantifier that was swapped (between `+` and `*`). Do not modify other quantifiers.