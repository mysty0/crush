# Antigravity CLI OAuth: reverse-engineering findings

## Why this exists

Google announced on 2026-05-19 that Gemini CLI and Gemini Code Assist IDE
extensions would stop serving free-tier and Google AI Pro/Ultra subscription
requests as of 2026-06-18, folding that functionality into a new unified
"Antigravity 2.0" product family (Antigravity desktop app, Antigravity IDE,
and Antigravity CLI, binary name `agy`). Crush's existing
`internal/oauth/geminicli` package implements the *old* Gemini CLI OAuth
client, which may stop working for consumer subscribers. This document
records what could be learned about Antigravity CLI's OAuth protocol so
Crush can add a parallel `antigravity` login path.

Sources: `https://developers.googleblog.com/an-important-update-transitioning-gemini-cli-to-antigravity-cli/`,
`https://github.com/google-antigravity/antigravity-cli`.

## Methodology

Antigravity CLI is distributed as a single ~173 MB statically-ish linked Go
binary via `curl -fsSL https://antigravity.google/cli/install.sh | bash`.
The install script downloads the binary from
`https://antigravity-cli-auto-updater-<project>.run.app`, verifies a
published SHA-512 checksum against a JSON manifest, and installs it — this
is Google's own official first-party distribution path, not a third party
mirror.

The environment this was run in (NixOS) cannot execute generic
dynamically-linked Linux binaries without `nix-ld`, so **the binary could
not actually be run**. All findings below come from `strings`-extracting
the downloaded `agy` v1.1.1 binary and reading adjacent literal strings,
Go symbol names, and protobuf field names compiled into it. This is the
same category of information already embedded in Crush's existing
`geminicli` package (whose client ID/secret were originally obtained the
same way from the published Gemini CLI npm package) — these are values
Google's own published binary ships to every user, not anything extracted
from a private or authenticated source.

**Because nothing was executed, no live HTTP exchange was observed at
extraction time.** Endpoint URLs, client credentials, and scopes below
are read directly from the binary and have reasonably high confidence;
the exact *shape* of the authorization-code redirect handoff was
initially inferred from string fragments and had lower confidence. A
first live login attempt (see "Live verification" below) has since
confirmed part of that shape and corrected one wrong guess.

## Confirmed (high confidence): literal strings extracted from the binary

**OAuth client registrations** (two distinct client id/secret pairs found,
both `apps.googleusercontent.com` "installed app"-style ids paired with
`GOCSPX-` prefixed secrets, the same public-secret model Google uses for
every installed-app OAuth client — including Gemini CLI's own, already
embedded in `internal/oauth/geminicli/geminicli.go`):

| Client ID | Client secret | Status |
|---|---|---|
| `884354919052-36trc1jjb3tguiac32ov6cod268c5blh.apps.googleusercontent.com` | `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` | **Confirmed live** (see below) — this is the pairing Crush now ships. |
| `1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com` | `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` | Unconfirmed; likely Antigravity IDE/desktop app rather than the CLI, going by elimination. |

`strings` alone could not prove which secret belongs to which client id
(they weren't co-located in memory in a way `strings` preserves). The
first shipped version of this package guessed the pairing by proximity in
the binary and got it backwards — cross-referencing the two rows above
against the paragraph immediately below shows why.

**OAuth/token endpoints:**
- `https://accounts.google.com/o/oauth2/auth` — authorization endpoint
  (the legacy non-versioned path; Google still serves it and redirects to
  the same consent flow as `/o/oauth2/v2/auth`, which is what
  `geminicli` uses).
- `https://oauth2.googleapis.com/token` — token endpoint (same as
  `geminicli`).
- `https://oauth2.googleapis.com/device/code` — RFC 8628 device
  authorization endpoint. Confirmed real: the string `authorization_pending`
  (the standard RFC 8628 polling-error code) is also present in the
  binary, which only makes sense if this endpoint is actually polled.
- `https://www.googleapis.com/oauth2/v2/userinfo` and
  `.../oauth2/v1/userinfo?alt=json` — profile lookup, same family
  `geminicli` uses.
- `https://antigravity.google/oauth-callback` — a **fixed HTTPS** redirect
  URI string, distinct from any `127.0.0.1` loopback address.

**Backend / inference routing** — identical to `geminicli`:
- `https://cloudcode-pa.googleapis.com` — same Cloud Code Assist backend
  Crush's `geminicli.BaseURL` already targets, including the same
  `v1internal:loadCodeAssist` / `v1internal:onboardUser` /
  `v1internal:retrieveUserQuota` RPC family (`cloudaicompanionProject`,
  `GetCloudaicompanionProject`, `ListCloudAICompanionProjects` all appear
  verbatim).
- `https://businessaicode.googleapis.com` and
  `https://daily-cloudcode-pa.googleapis.com` appear as fallback/staging
  hosts.
- `https://gaiastaging.corp.google.com/o/oauth2/{auth,token}` also appear,
  but under a `corp.google.com` domain reachable only to Google
  employees/internal testing — not a path external users would hit.

**Scopes requested** (beyond the three `geminicli` already requests —
`cloud-platform`, `userinfo.email`, `userinfo.profile`): additional
`https://www.googleapis.com/auth/drive*` scopes (`drive`, `drive.readonly`,
`drive.metadata`, `drive.appdata`, `drive.scripts`, `drive.apps.readonly`,
`drive.meet.readonly`) and `.../auth/cclog`,
`.../auth/experimentsandconfigs` appear, presumably for Antigravity's
Docs/Drive integration and internal telemetry. These are broader than what
coding-assistant inference needs.

**UI/error strings confirming the general flow shape:**
- `"Error: Please sign in to view available models. Launch the CLI without
  arguments to sign in."` — interactive launch triggers auth if not logged
  in.
- `"Authentication required. Please visit the URL to log in:"` and
  `"If you aren't automatically redirected, paste the authorization code
  below:"` — confirms a browser-based authorization-code flow with a
  manual code-paste fallback, consistent with a fixed HTTPS redirect page
  that can't always hand the code back to the local process automatically.
- `"Print mode: not authenticated, trying silent auth"` /
  `"Print mode: submitting manually-entered auth code"` /
  `"Print mode: auth cancelled or interrupted"` — non-interactive mode
  supports the same manual-code flow.
- `"3. Or unset USE_ADC env var to use other sign in methods."` —
  Application Default Credentials is also an accepted auth source.
- `"LoginWithBrowser is deprecated and no longer supported"` — an old gRPC
  RPC of that name exists in the shared language-server protocol (carried
  over from the Codeium/Windsurf codebase this product is built on) but is
  explicitly dead; the real login path lives elsewhere in the CLI, not
  behind that RPC.

## Live verification (first real login attempt)

A user ran `crush login antigravity` (browser flow, port 8086) against the
first shipped version of this package. Results, in order:

1. `https://accounts.google.com/o/oauth2/auth` accepted the request —
   including `redirect_uri=http://127.0.0.1:8086/oauth2callback` — and
   redirected back to the loopback server with an authorization code.
   **This resolves the central open question below: the client is
   registered as a "Desktop app" / installed-app OAuth client per
   RFC 8252, and Google accepts an arbitrary loopback port.** The fixed
   `antigravity.google/oauth-callback` page is therefore not required for
   the CLI flow; it's likely only used by other Antigravity surfaces (IDE,
   desktop app) or as a documentation/fallback page.
2. The subsequent `POST https://oauth2.googleapis.com/token`
   authorization-code exchange failed with `401 invalid_client: The
   provided client secret is invalid.` — confirming the *client_id* is
   correct and live, but the guessed client_id/secret pairing (picked by
   proximity in the binary, see above) was wrong.
3. Swapping to the other secret extracted from the binary
   (`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`) is the current best guess,
   shipped now; **it has not yet been confirmed by a successful full
   token exchange.** If it also fails, the two secrets should be
   swapped back and the *other* client_id tried instead, since it's
   possible the CLI's actual client_id is the second one
   (`1071006060591-tmhssin2h21lcre235vtolojh4g403ep...`) rather than the
   one confirmed live here — Google's authorize endpoint would happily
   accept either client_id at step 1 as long as the redirect_uri matches
   *some* registered pattern for it, so step 1 succeeding doesn't by
   itself prove which of the two client_ids is "the CLI's".

## Resolved by live testing

~~The central open question was how the fixed
`antigravity.google/oauth-callback` redirect hands the authorization code
back to the local `agy` process, since Google's OAuth server requires the
`redirect_uri` sent to the authorize endpoint to exactly match one
pre-registered for the client.~~ **Resolved**: see "Live verification"
above — the client accepts an arbitrary `127.0.0.1` loopback redirect, so
a bare loopback flow shaped like `geminicli.LoginBrowser` is the right
approach, and this package's `LoginBrowser` needs no further changes on
that front.

## Still open

- **Client secret**: **confirmed working** by the second live login
  attempt — token exchange succeeded end to end (see "Live verification,
  round 2" below).
- **`pluginType`/`ideType` identity**: **resolved by direct schema
  extraction**, not guessing — see "Live verification, round 3" below.
  Unverified only in the sense that the corrected identity has not yet
  been tried against a live login.
- **Device flow**: `LoginDevice` (`oauth2.googleapis.com/device/code`)
  has not been exercised at all yet. RFC 8628 device flow doesn't involve
  `redirect_uri` matching, so it's expected to work once the tier issue
  is sorted — but this is still unverified.
- **Scopes**: whether the narrowed 3-scope set (vs. the full set including
  Drive scopes the real CLI requests) causes any behavioral difference in
  onboarding/inference is unverified.

## Live verification, round 2: client secret confirmed, new tier problem

With the corrected secret, `crush login antigravity` completed the full
OAuth token exchange successfully — no more `invalid_client`. The failure
moved one step further, into Cloud project discovery:

```
Discovering Cloud Code Assist project...
ERROR
Geminicli: tier "standard-tier" requires a Cloud project; set GOOGLE_CLOUD_PROJECT.
```

This is a **different kind of failure than an OAuth/wire-format bug**: it
comes from Google's `loadCodeAssist` RPC itself reporting that this
account (a Google AI Pro/Ultra consumer subscriber, not a GCP org/Workspace
account) is on `"standard-tier"`, which Crush's (and Gemini CLI's own)
discovery logic treats as requiring an explicit `GOOGLE_CLOUD_PROJECT` —
correct behavior for a GCP-project-backed tier, but not what a personal
AI-subscription account should need.

**Hypothesis**: Google's tier-eligibility check for `loadCodeAssist` most
plausibly keys off the `pluginType` field in the request's `ClientMetadata`
block (`ideType`/`platform`/`pluginType`/`duetProject`), which until now
Crush hardcoded to `"GEMINI"` (copied from `geminicli`, since the wire
format is otherwise identical). This lines up exactly with the
announcement motivating this whole package: Google said it stopped
serving Pro/Ultra subscribers through the Gemini CLI identity specifically
— i.e. requests declaring `pluginType=GEMINI` may now always resolve to
the generic `"standard-tier"` fallback regardless of the account's actual
consumer subscription, while the new Antigravity identity is presumably
what's wired to recognize that subscription and return `"free-tier"` (no
project required) instead.

Initial supporting evidence from `strings` output: a `ClientMetadata_PluginType`
protobuf field confirmed pluginType is a real enum, not a free string, but
`strings` alone didn't reveal the enum's value names — those turned out
to be reachable, just not via `strings`; see round 3 below. At the time
this fix shipped, no confirmed evidence for the exact string existed;
**`"ANTIGRAVITY"` was a best-effort guess** based on the naming
convention Gemini CLI itself uses (`pluginType=GEMINI`, product name
"GeminiCLI").

**Fix shipped in this round, later found wrong (see round 3)**:
`internal/oauth/geminicli` was given an `Identity{Product, Version,
PluginType}` parameter threaded through `DiscoverProject`, `FetchUsage`,
`cliHeaders`, and `WireTransport`, instead of hardcoding
`"GEMINI"`/`"GeminiCLI"` everywhere. Gemini CLI's own login/inference path
explicitly passes `GeminiCLIIdentity` (behavior unchanged). The new
`antigravity` package defined its own `Identity` with `PluginType:
"ANTIGRAVITY"` — this specific value turned out to be wrong, corrected in
round 3 below.

## Live verification, round 3: exact schema recovered, root cause confirmed

The third login attempt, with `pluginType="ANTIGRAVITY"`, got past OAuth
entirely and reached `loadCodeAssist`, which Google's server rejected
outright:

```
loadCodeAssist failed: Bad Request - {
  "error": {
    "code": 400,
    "message": "Invalid value at 'metadata.plugin_type' (...ClientMetadata.PluginType), \"ANTIGRAVITY\"",
    "status": "INVALID_ARGUMENT", ...
  }
}
```

This is a clean, unambiguous signal (exactly what round 2 predicted it
would look like if the guess were wrong): `pluginType` is a strict enum
and `"ANTIGRAVITY"` isn't a member.

**Root cause found by extracting the raw protobuf schema, not by
guessing again.** `ClientMetadata_PluginType`'s value names are not
plaintext-extractable via the `strings` tool (they're embedded inside a
serialized, non-gzipped `FileDescriptorProto` byte blob, which mixes
UTF-8 field names with binary protobuf tag/length bytes that break up
naive text scanning). Locating the byte offset of the `ClientMetadata`
message descriptor directly in the binary and dumping raw bytes around it
(rather than running `strings`) revealed the two enums verbatim:

```
enum IdeType {
  IDE_UNSPECIFIED = 0; VSCODE = 1; INTELLIJ = 2;
  VSCODE_CLOUD_WORKSTATION = 3; INTELLIJ_CLOUD_WORKSTATION = 4;
  CLOUD_SHELL = 5; CIDER = 6; CLOUD_RUN = 7; ANDROID_STUDIO = 8;
  ANTIGRAVITY = 9; JETSKI = 10; COLAB = 11; FIREBASE = 12;
  CHROME_DEVTOOLS = 13; GEMINI_CLI = 14;
}

enum PluginType {
  PLUGIN_UNSPECIFIED = 0;
  CLOUD_CODE = 1;
  GEMINI = 2 [deprecated = true];
  AIPLUGIN_INTELLIJ = 3;
  AIPLUGIN_STUDIO = 4;
  PANTHEON = 6;
}
```

This resolves the mystery cleanly:
- **`"ANTIGRAVITY"` is a real, valid enum value — but of `IdeType`, not
  `PluginType`.** The round-2 guess put it in the wrong field.
- **`PluginType.GEMINI` is explicitly marked `deprecated = true`** in
  Google's own schema. This is the most direct confirmation yet of the
  root cause: Gemini CLI's identity is deprecated server-side, which is
  almost certainly why it resolves Pro/Ultra consumer accounts to the
  restrictive `"standard-tier"` instead of a free/consumer tier.
- **There is no dedicated CLI `PluginType` value at all.** The only
  non-deprecated, generic option is `CLOUD_CODE`. The per-product
  identity is therefore carried by `IdeType` (and possibly the sibling
  freeform `ide_name` string field also present on `ClientMetadata`, not
  yet used), not by `PluginType`.

**Fix shipped**: `geminicli.Identity` gained an `IDEType` field (defaults
to `"IDE_UNSPECIFIED"`, preserving Gemini CLI's existing behavior exactly).
`antigravity.Identity` now sets `PluginType: "CLOUD_CODE"` (real,
non-deprecated) and `IDEType: "ANTIGRAVITY"` (real enum value, correct
field this time). This pairing is grounded directly in the server's own
schema rather than pattern-matching a naming convention, but has not yet
been exercised against a live `loadCodeAssist` call — that's the next
thing to verify.

## Live verification, round 4: real root cause found by disassembling the actual binary

After round 3's fix (correct `pluginType`/`ideType`) produced the *exact
same* `standard-tier` result as before, that ruled out client identity as
the cause entirely: changing it changed nothing, so Google's tier
assignment must depend on something else about the request. At this
point static string extraction had run out of runway, so the investigation
switched to actually disassembling the real `agy` v1.1.1 binary instead of
reading strings out of context.

**Tooling note**: this environment (NixOS) cannot execute arbitrary
dynamically-linked Linux binaries, and even Ghidra's own headless
auto-analysis includes a real disassembler (objdump/radare2 work directly
on any ELF regardless of NixOS's execution restriction — only *running*
the binary is blocked, not reading and disassembling its bytes). Ghidra
12.0.4 was installed via Nix and run headlessly against the binary; its
built-in Go-aware analyzer ("Golang Symbols") refused to run
(`Untested Go version [1.27.0]`, newer than anything that Ghidra release
recognizes), so it could not auto-recover Go symbol names or types.
Instead, the binary's Go runtime function-name table (`pclntab`) was
parsed directly (a well-documented, stable format independent of Ghidra's
Go-version allowlist) to recover the true address of every named Go
function in the binary — including
`google3/third_party/jetski/cli/backend/auth/auth.getOauthParams` and
`.getScopesByAuthMethod`, the exact functions that build the OAuth
request — and `objdump` was used to disassemble their real machine code
directly from the file.

**What the disassembly shows, byte-for-byte confirmed:**

`getOauthParams` compares its `authMethod` string argument against the
3-byte literal `"gcp"` (checked via a 2-byte + 1-byte immediate
comparison, not just a visual string match) and branches:

```
if authMethod == "gcp":
    clientID     = <72-byte string at 0x4904824>
    clientSecret = <35-byte string at 0x488cb37>
else:  # includes "consumer", confirmed via the sibling function below
    clientID     = <73-byte string at 0x490620b>
    clientSecret = <35-byte string at 0x488cb14>
```

The exact byte lengths compared in the machine code (0x48=72 and
0x49=73) were checked against the real string data at each address and
match exactly — this is not a guess or a visual read of `strings` output,
it is the literal operand the compiled comparison uses:

| Auth method | Client ID (verified length) | Secret (verified length) |
|---|---|---|
| `"gcp"` | `884354919052-36trc1jjb3tguiac32ov6cod268c5blh.apps.googleusercontent.com` (72) | `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` (35) |
| fallback (`"consumer"`) | `1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com` (73) | `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` (35) |

The sibling function `getScopesByAuthMethod` independently confirms the
`"consumer"` name: it contains a `movabs` immediate load of the 8-byte
value `0x72656d75736e6f63`, which is the literal ASCII bytes of
`"consumer"` packed into a single 64-bit comparison (the standard
small-string-literal compilation pattern), used in an equivalent branch.

**This exactly explains the entire history of this investigation**:
- Round 1's very first login used client_id `884354919052...` (the `"gcp"`
  id) together with secret `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` (the
  `"consumer"` secret) — a cross-mismatched pairing between the two
  methods. That is exactly why it failed with `invalid_client`.
- Round 2 "fixed" it by pairing `884354919052...` with its correct secret
  `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` — which is precisely the `"gcp"`
  method's registration. It passed OAuth cleanly (both the client_id and
  the pairing were now internally consistent) but every subsequent call
  behaved exactly like a GCP-project account, because it *was* using the
  GCP-oriented OAuth client the whole time — hence `standard-tier` and
  the demand for `GOOGLE_CLOUD_PROJECT`.
- Round 3's `pluginType`/`ideType` change had no effect because the tier
  problem was never about client identity metadata at all — it was about
  which of the two entirely separate OAuth *clients* was used to obtain
  the token in the first place.

**Supporting context from `chainedAuthOrDefault`** (the function that
assembles the CLI's list of available auth methods on startup): it
constructs exactly two entries using these same two client_id/secret
string addresses verified above, confirming both pairs are real,
in-use registrations rather than dead code or leftover constants. The
precise struct field layout (and therefore which of the two is tried
first / is the default for an interactive `agy` login) could not be
fully resolved without proper Go type information, which Ghidra's
analyzer could not provide for this Go version — but the `"consumer"`
name itself is a strong signal on its own: it is the auth method whose
name directly matches what a personal Google account doing Google AI
Pro/Ultra subscription billing would use, as opposed to `"gcp"` which
matches a Google Cloud Platform organization/project-billed account —
exactly the distinction this whole investigation has been trying to
find.

**Fix shipped**: `internal/oauth/antigravity` now uses the `"consumer"`
pairing — client_id `1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com`
with secret `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` — in place of the
`"gcp"` pairing used since round 2. This is the strongest evidence-based
fix so far: unlike the client-identity metadata changes in round 3, this
is a confirmed, real, alternate OAuth *client* found in the actual binary
logic, not a plausible-sounding schema value. It has not yet been
exercised against a live login.

## Recommendation

Ship Crush's `internal/oauth/antigravity` package as **experimental**:
- OAuth login (loopback flow, both authorize and token exchange) is
  confirmed working end to end against the `"gcp"` client; the
  `"consumer"` client shares the same loopback redirect handling and
  should work identically at the OAuth layer — only the credentials
  differ.
- The next login attempt should confirm whether the `"consumer"` client
  resolves the account to a project-free tier (the actual goal of this
  entire investigation) rather than `standard-tier`. If it still returns
  `standard-tier`, the tier gate is likely a genuine property of the
  Google account itself (e.g. whether it has ever touched a GCP project)
  rather than anything client-side, and no further OAuth-level change is
  likely to fix it — see "Live verification, round 3" for the reasoning
  that ruled out client identity as the sole cause.
- Keep `--device` available as a fallback/headless option; it has not
  been exercised yet, but the device flow doesn't depend on
  `redirect_uri` matching, so it should work with either client pairing
  once the browser flow is confirmed.
- Continue reusing `internal/oauth/geminicli`'s `DiscoverProject`,
  `WireTransport`, and `FetchUsage`, parameterized by
  `Identity{Product, Version, PluginType, IDEType}` — the RPCs, wire
  envelope, and backend host are confirmed identical to Gemini CLI's;
  only the OAuth client and identity metadata differ.
- Do not request the broader Drive scopes; Crush only needs inference
  access, and requesting scopes it doesn't use would both widen the
  consent screen unnecessarily and risk tripping Google's sensitive-scope
  review for an unverified reverse-engineered client usage.

This should be revisited after the next login attempt confirms or refutes
the `"consumer"` client pairing, and again once `LoginDevice` and
inference (`WireTransport`) have been exercised end-to-end.
