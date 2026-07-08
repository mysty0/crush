# Fix the bug in `rubygems.ts`

Optional chaining was removed from a property access.

The issue is on line 89.

Restore the optional chaining operator (`?.`) at the ONE location where it was removed. Do not add optional chaining elsewhere.