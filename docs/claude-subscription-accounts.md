# Claude subscription accounts

Crush can drive a Claude Pro/Max subscription directly, and can hold
several subscription accounts at once — a personal plan and a work plan,
say — switching between them the same way you switch models.

## Logging in

```sh
crush login claude                     # the default account
crush login claude --account work      # a second account, alongside the first
crush login claude --manual            # headless: paste the code instead of
                                       # listening for a browser redirect
```

Each login runs the PKCE authorization-code flow against Anthropic's
subscription OAuth endpoint and stores the resulting token in Crush's
global config. Tokens are refreshed automatically, per account, and the
refresh is single-flighted across sessions and processes.

## How accounts are addressed

Every account is its own provider:

| Account          | Provider id         | Shown as               |
| ---------------- | ------------------- | ---------------------- |
| default          | `claude-code`       | `Claude Code`          |
| `--account work` | `claude-code-work`  | `Claude Code (work)`   |

Because they are ordinary providers, everything downstream works without
special cases: pick the account you want in the model list, pin one to a
specific agent in `crush.json`, or check each account's remaining limits
in the plan-usage dialog, which shows one section per account.

Account names are normalized into the provider id — `--account "Work
Laptop"` becomes `claude-code-work-laptop`.

Logging in twice with the same Anthropic account under two names is
reported at login: two providers pointing at one subscription share a
single rate limit, which defeats the point of a second account.

## Signing out

```sh
crush logout claude                # the default account
crush logout claude-code-work      # a named account, by provider id
crush logout                       # pick from the logged-in list
```

## The default account and the Claude Code CLI

The `claude-code` provider predates this flow: when it holds no token of
its own, it authenticates from `~/.claude/.credentials.json`, the file the
official Claude Code CLI maintains (override with `$CLAUDE_CREDENTIALS`).
That keeps working untouched — existing configs need no changes.

Running `crush login claude` gives that provider a token of its own, after
which Crush stops consulting the credentials file for it. Named accounts
always use their own stored token; only the default account can fall back
to the file.

## Tuning an account

A logged-in account needs no `crush.json` block, but you can still add one
to override anything except the endpoint, provider type, and model list,
which Crush owns:

```json
{
  "providers": {
    "claude-code-work": {
      "name": "Work plan",
      "flat_rate": true
    }
  }
}
```

`flat_rate` skips cost accumulation, which is usually what you want for
subscription traffic — it is billed against the plan, not per token.

## Model lists

Model discovery runs per account against `/v1/models`, so two accounts on
different plans each show the line-up their own subscription offers. The
result is cached for an hour per account, falling back to a built-in list
when the endpoint cannot be reached.
