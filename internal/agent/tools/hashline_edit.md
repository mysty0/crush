Applies source edits using the hashline patch language. Input is one or more `[PATH#TAG]` file sections; each names lines to replace, delete, or insert at, then lists the new content. A header ending in `:` is followed by `+` body rows; `DEL` takes no body.

Headers: every section starts with `[PATH#TAG]`. `TAG` is the 4-hex tag from your latest `Read` of that file — REQUIRED, and rejected if the file changed since. Create new files with the Write tool; hashline only edits existing files.

Operations:
- `SWAP N.=M:` — replace original lines N..M (inclusive) with the body rows below.
- `SWAP.BLK N:` — replace the whole syntactic block that begins on line N (resolved by tree-sitter).
- `DEL N.=M` — delete original lines N..M. No body. `DEL N` deletes one line.
- `DEL.BLK N` — delete the block that begins on line N.
- `INS.PRE N:` / `INS.POST N:` — insert body rows immediately before / after line N.
- `INS.HEAD:` / `INS.TAIL:` — insert body rows at the very start / end of the file.
- `INS.BLK.POST N:` — insert body rows after the end of the block that begins on line N.
- `REM` — delete the whole file named by the section header (no body, no line ops).
- `MV DEST` — rename/move the section file to DEST (line edits, if any, apply first, then the result is written at DEST).

Body rows: every body row is `+TEXT` (verbatim; `+` alone adds a blank line). Never write `-old` or a bare context line. To keep a line, leave it out of every range. Literal lines starting with `-`/`+` still need the prefix: Markdown `- item` -> `+- item`.

Rules:
- Line numbers and the `[PATH#TAG]` header come from your latest `Read` (`LINE:TEXT` rows). They refer to the ORIGINAL file and never shift as hunks apply.
- Every applied edit mints a fresh `#TAG` and renumbers — anchor the next edit on the edit response or a fresh `Read`.
- Touch only lines your latest `Read` literally displayed as `LINE:TEXT`; the tag certifies the snapshot, not your memory.
- **Duplicate/similar lines:** Seeing a line of text is not the same as it holding the code you mean. When the same (or near-identical) text appears multiple times in a file, confirm the line number maps to the exact construct you intend BEFORE issuing any `SWAP` or `DEL`. Narrow your `Read` (or `grep`) to the surrounding lines and verify the correct occurrence first.
- Ranges cover ONLY lines whose content changes. Never widen over unchanged lines — a wide range shreds everything it spans.
- Pure additions use `INS.*`, never a widened `SWAP`.
- One hunk per range; the body is the final content, never an old/new pair.
- On a stale-tag rejection or any surprising result: STOP and re-`Read` before further edits.
- Whole construct -> `SWAP.BLK N` (tree-sitter resolves the end); specific lines inside it -> `SWAP N.=M`.

Example. Given this Read output:
```
[greet.py#A1B2]
1:def greet(name):
2:    msg = "Hello, " + name
3:    print(msg)
```
Replace line 2 with two lines:
```
[greet.py#A1B2]
SWAP 2.=2:
+    greeting = "Hi"
+    msg = f"{greeting}, {name}"
```
Insert after line 1:
```
[greet.py#A1B2]
INS.POST 1:
+    if not name: name = "stranger"
```
Delete line 3:
```
[greet.py#A1B2]
DEL 3
```

Multiple files are edited in one call by stacking sections; the whole batch is validated before anything is written.

If you remember nothing else:
1. **RE-GROUND AFTER EVERY EDIT.** Every apply mints a fresh `#TAG` and renumbers — take the next edit's numbers from the edit response or a fresh `Read`. Stale tag or surprise? STOP, re-`Read`.
2. **RANGES ARE TIGHT.** Cover only lines that change; a wide range shreds everything it spans. Whole construct → `SWAP.BLK N`.
3. **CONFIRM LINE IDENTITY BEFORE EDITING.** When a target line is one of several identical or similar lines, use `Read` with a narrow offset/limit or `grep` to confirm you have the right occurrence before you commit the patch.
4. **THE BODY IS THE FINAL CONTENT.** Every body row starts with `+`; no `-` rows, no context lines.
