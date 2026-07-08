# Fix the bug in `orcid.ts`

A guard clause (early return) was removed.

The issue is in the `formatDate` function.

Restore the missing guard clause (if statement with early return). Add back the exact 3-line pattern: if condition, return statement, closing brace.