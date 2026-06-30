#!/usr/bin/env bash
#
# crush-token.sh
#
# Bridges Crush to the Claude Code (Pro/Max) subscription OAuth token.
#
# It reads the Claude Code credentials JSON, refreshes the access token when it
# is expired or within 5 minutes of expiring, persists the refreshed token back
# to the credentials file atomically, and prints "Bearer <accessToken>" to
# stdout (and nothing else). All diagnostics go to stderr.
#
# Usage (from crush.json):
#   "api_key": "$(bash /abs/path/to/crush-token.sh)"
#
# Requirements: bash, jq, curl.
#
# Environment:
#   CLAUDE_CREDENTIALS  Override path to the credentials file.
#                       Defaults to "$HOME/.claude/.credentials.json".

set -euo pipefail

CLIENT_ID="9d1c250a-e61b-44d9-88ed-5944d1962f5e"
TOKEN_URL="https://platform.claude.com/v1/oauth/token"
# Refresh when the token expires within this many milliseconds (5 minutes).
REFRESH_WINDOW_MS=300000

CREDS="${CLAUDE_CREDENTIALS:-$HOME/.claude/.credentials.json}"

err() { printf 'crush-token: %s\n' "$*" >&2; }

command -v jq >/dev/null 2>&1 || { err "jq is required but was not found in PATH"; exit 1; }
command -v curl >/dev/null 2>&1 || { err "curl is required but was not found in PATH"; exit 1; }

if [[ ! -f "$CREDS" ]]; then
  err "credentials file not found: $CREDS"
  exit 1
fi

now_ms=$(( $(date +%s) * 1000 ))

access_token="$(jq -r '.claudeAiOauth.accessToken // empty' "$CREDS")"
refresh_token="$(jq -r '.claudeAiOauth.refreshToken // empty' "$CREDS")"
expires_at="$(jq -r '.claudeAiOauth.expiresAt // 0' "$CREDS")"
scopes="$(jq -r '(.claudeAiOauth.scopes // []) | join(" ")' "$CREDS")"
if [[ -z "$scopes" ]]; then
  scopes="user:inference user:profile"
fi

if [[ -z "$refresh_token" ]]; then
  err "no refreshToken present in $CREDS"
  exit 1
fi

# Refresh when the token is already expired or within the refresh window.
if (( expires_at - now_ms <= REFRESH_WINDOW_MS )); then
  err "access token expired or near expiry; refreshing"

  body="$(jq -n \
    --arg rt "$refresh_token" \
    --arg cid "$CLIENT_ID" \
    --arg scope "$scopes" \
    '{grant_type: "refresh_token", refresh_token: $rt, client_id: $cid, scope: $scope}')"

  response="$(curl -fsS -X POST "$TOKEN_URL" \
    -H "Content-Type: application/json" \
    -d "$body")" || { err "token refresh request failed"; exit 1; }

  new_access="$(jq -r '.access_token // empty' <<<"$response")"
  new_refresh="$(jq -r '.refresh_token // empty' <<<"$response")"
  expires_in="$(jq -r '.expires_in // empty' <<<"$response")"

  if [[ -z "$new_access" || -z "$expires_in" ]]; then
    err "refresh response missing access_token/expires_in"
    exit 1
  fi

  # Keep the existing refresh token if the server did not rotate it.
  [[ -z "$new_refresh" ]] && new_refresh="$refresh_token"

  new_expires_at=$(( now_ms + expires_in * 1000 ))

  # Write atomically: render into a temp file in the same directory, then mv.
  tmp="$(mktemp "${CREDS}.XXXXXX")"
  if jq \
    --arg at "$new_access" \
    --arg rt "$new_refresh" \
    --argjson ea "$new_expires_at" \
    '.claudeAiOauth.accessToken = $at
     | .claudeAiOauth.refreshToken = $rt
     | .claudeAiOauth.expiresAt = $ea' \
    "$CREDS" >"$tmp"; then
    mv -f "$tmp" "$CREDS"
  else
    rm -f "$tmp"
    err "failed to write refreshed credentials"
    exit 1
  fi

  access_token="$new_access"
fi

if [[ -z "$access_token" ]]; then
  err "no accessToken available"
  exit 1
fi

printf 'Bearer %s\n' "$access_token"
