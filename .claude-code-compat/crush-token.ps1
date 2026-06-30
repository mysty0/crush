#!/usr/bin/env pwsh
#Requires -Version 7.0
#
# crush-token.ps1
#
# Windows-native (PowerShell 7) equivalent of crush-token.sh. No jq dependency.
#
# Reads the Claude Code credentials JSON, refreshes the access token when it is
# expired or within 5 minutes of expiring, persists the refreshed token back to
# the credentials file atomically, and prints exactly "Bearer <accessToken>" to
# stdout. All diagnostics go to stderr.
#
# Usage (from crush.json on Windows):
#   "api_key": "$(pwsh -File C:/abs/path/to/crush-token.ps1)"
#
# Environment:
#   CLAUDE_CREDENTIALS  Override path to the credentials file.
#                       Defaults to "$HOME/.claude/.credentials.json".

$ErrorActionPreference = 'Stop'

$ClientId        = '9d1c250a-e61b-44d9-88ed-5944d1962f5e'
$TokenUrl        = 'https://platform.claude.com/v1/oauth/token'
# Refresh when the token expires within this many milliseconds (5 minutes).
$RefreshWindowMs = 300000

function Write-Err([string]$msg) { [Console]::Error.WriteLine("crush-token: $msg") }

$creds = if ($env:CLAUDE_CREDENTIALS) {
    $env:CLAUDE_CREDENTIALS
} else {
    Join-Path $HOME '.claude/.credentials.json'
}

if (-not (Test-Path -LiteralPath $creds)) {
    Write-Err "credentials file not found: $creds"
    exit 1
}

try {
    $json = Get-Content -LiteralPath $creds -Raw | ConvertFrom-Json
} catch {
    Write-Err "failed to parse credentials JSON: $_"
    exit 1
}

$oauth = $json.claudeAiOauth
if (-not $oauth) {
    Write-Err "credentials file missing claudeAiOauth object"
    exit 1
}

$accessToken  = $oauth.accessToken
$refreshToken = $oauth.refreshToken
$expiresAt    = [int64]($oauth.expiresAt)

$scopes = if ($oauth.scopes) { ($oauth.scopes -join ' ') } else { '' }
if ([string]::IsNullOrWhiteSpace($scopes)) { $scopes = 'user:inference user:profile' }

if ([string]::IsNullOrEmpty($refreshToken)) {
    Write-Err "no refreshToken present in $creds"
    exit 1
}

$nowMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()

# Refresh when the token is already expired or within the refresh window.
if (($expiresAt - $nowMs) -le $RefreshWindowMs) {
    Write-Err "access token expired or near expiry; refreshing"

    $body = @{
        grant_type    = 'refresh_token'
        refresh_token = $refreshToken
        client_id     = $ClientId
        scope         = $scopes
    } | ConvertTo-Json -Compress

    try {
        $resp = Invoke-RestMethod -Method Post -Uri $TokenUrl -ContentType 'application/json' -Body $body
    } catch {
        Write-Err "token refresh request failed: $_"
        exit 1
    }

    if (-not $resp.access_token -or -not $resp.expires_in) {
        Write-Err "refresh response missing access_token/expires_in"
        exit 1
    }

    $accessToken = $resp.access_token
    # Keep the existing refresh token if the server did not rotate it.
    if ($resp.refresh_token) { $refreshToken = $resp.refresh_token }
    $newExpiresAt = $nowMs + [int64]$resp.expires_in * 1000

    $oauth.accessToken  = $accessToken
    $oauth.refreshToken = $refreshToken
    $oauth.expiresAt    = $newExpiresAt

    # Write atomically: render into a temp file in the same directory, then move.
    $tmp = "$creds.$([System.Guid]::NewGuid().ToString('N')).tmp"
    try {
        $json | ConvertTo-Json -Depth 32 | Set-Content -LiteralPath $tmp -Encoding utf8
        Move-Item -LiteralPath $tmp -Destination $creds -Force
    } catch {
        if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Force }
        Write-Err "failed to write refreshed credentials: $_"
        exit 1
    }
}

if ([string]::IsNullOrEmpty($accessToken)) {
    Write-Err "no accessToken available"
    exit 1
}

[Console]::Out.Write("Bearer $accessToken")
