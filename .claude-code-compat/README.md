# Claude Code subscription auth for Crush

This directory lets Crush authenticate to Anthropic using the **Claude Pro/Max
subscription OAuth token that the Claude Code CLI already stores** — no separate
Anthropic API key required.

## How it works

The Claude Code CLI stores its OAuth credentials at
`~/.claude/.credentials.json`:

```json
{
  "claudeAiOauth": {
    "accessToken": "...",
    "refreshToken": "...",
    "expiresAt": 1782716484251,
    "scopes": ["user:inference", "user:profile", "..."],
    "subscriptionType": "max",
    "rateLimitTier": "..."
  }
}
```

A small helper script (`crush-token.sh`, or `crush-token.ps1` on Windows):

1. Reads that credentials file.
2. If the `accessToken` is expired or within 5 minutes (300000 ms) of expiring,
   it refreshes it via `POST https://platform.claude.com/v1/oauth/token` and
   writes the new `accessToken` / `refreshToken` / `expiresAt` back to the
   credentials file **atomically** (temp file + move). The rest of the file
   (e.g. `subscriptionType`, `scopes`, `rateLimitTier`) is preserved.
3. Prints `Bearer <accessToken>` to stdout (and nothing else; all diagnostics
   go to stderr).

`crush.json` defines a provider named `claude-code` whose `api_key` is set to
`$(bash .../crush-token.sh)`. Crush resolves `$(...)` command substitution in
`api_key` at startup, so the provider always receives a fresh `Bearer <token>`.

Because Crush's Anthropic provider treats an `api_key` that starts with
`Bearer ` as an `Authorization` header (instead of `x-api-key`), and because the
`extra_headers` add the magic `anthropic-beta: oauth-2025-04-20` beta, the
subscription token is accepted as a first-class OAuth credential.

## Requirements

- `bash`, [`jq`](https://jqlang.github.io/jq/), and `curl` on `PATH`
  (the macOS/Linux path). On Windows use the PowerShell 7 (`pwsh`) script
  instead — it has no `jq`/`curl` dependency.
- A logged-in Claude Code CLI (so `~/.claude/.credentials.json` exists).

## Install

### 1. Make the helper executable (macOS / Linux / WSL / Git Bash)

```sh
chmod +x /abs/path/to/crush/.claude-code-compat/crush-token.sh
```

### 2. Point `crush.json` at the absolute path of the helper

Edit `crush.json` in this directory and replace the path in `api_key` with the
absolute path on **your** machine. The committed value is:

```json
"api_key": "$(bash F:/projects/code-agent/crush/.claude-code-compat/crush-token.sh)"
```

On Windows, reference the PowerShell helper instead:

```json
"api_key": "$(pwsh -File F:/projects/code-agent/crush/.claude-code-compat/crush-token.ps1)"
```

### 3. Put `crush.json` where Crush looks for config

Crush merges config from several locations (later = higher priority). Pick one:

- **Global user config:** `~/.config/crush/crush.json`
  (this path is used on all platforms, including Windows; overridable with the
  `CRUSH_GLOBAL_CONFIG` environment variable). This is the best place if you
  want `claude-code` available in every project.
- **Project config:** `crush.json` or `.crush.json` in your project's working
  directory (Crush walks up to the git-repo root looking for these).
- **Project workspace config:** `<data_dir>/crush.json`, i.e.
  `.crush/crush.json` inside the project (highest priority, machine-local).

For a global install you can merge the provider/model blocks from this
directory's `crush.json` into `~/.config/crush/crush.json`. Keep the
`$schema` line. Remember to keep the absolute helper path correct.

## Verify the helper directly

```sh
bash /abs/path/to/crush-token.sh        # prints: Bearer sk-ant-oat01-...
# or on Windows:
pwsh -File C:/abs/path/to/crush-token.ps1
```

You can point either script at a different credentials file with the
`CLAUDE_CREDENTIALS` environment variable.

## Known limitations

- **Token is resolved once per Crush process start.** Crush resolves the
  `api_key` command substitution when it loads config (at launch). If a session
  runs longer than the token's remaining lifetime, the in-memory `Bearer` token
  is **not** automatically refreshed mid-process — restart Crush to pick up a
  fresh token. The on-disk credentials file is still refreshed and rotated
  correctly by the helper on the next launch. (The 5-minute early-refresh window
  reduces, but does not eliminate, the chance of starting a session with a
  token that expires soon.)
- This relies on Anthropic's subscription OAuth endpoints and the
  `anthropic-beta: oauth-2025-04-20` beta, which are not officially documented
  and may change.
