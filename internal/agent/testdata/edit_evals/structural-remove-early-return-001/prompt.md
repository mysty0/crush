# Fix the bug in `pirate.ts`

A guard clause (early return) was removed.

The issue starts around line 28.

Restore the missing guard clause (if statement with early return). Add back the exact 3-line pattern: if condition, return statement, closing brace.