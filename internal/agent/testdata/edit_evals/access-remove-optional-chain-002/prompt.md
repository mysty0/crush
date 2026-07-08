# Fix the bug in `render.ts`

Optional chaining was removed from a property access.

The issue is around the middle of the file.

Restore the optional chaining operator (`?.`) at the ONE location where it was removed. Do not add optional chaining elsewhere.