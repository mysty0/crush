#!/usr/bin/env pwsh
# Query the Claude Code subscription's available models from the Anthropic
# /v1/models endpoint and write them into a Crush config's `claude-code`
# provider `models` array, so they all show up in the Crush model picker.
#
# Usage:
#   pwsh -File refresh-models.ps1 [path-to-crush.json]
# Defaults to the global config at ~/.config/crush/crush.json.

param(
    [string]$ConfigPath = (Join-Path $HOME ".config/crush/crush.json")
)

$ErrorActionPreference = 'Stop'
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path

# Reuse the token helper (handles refresh) and strip the "Bearer " prefix.
$bearer = & pwsh -File (Join-Path $scriptDir "crush-token.ps1")
$token  = $bearer -replace '^Bearer\s+', ''

$resp = Invoke-RestMethod -Method Get -Uri "https://api.anthropic.com/v1/models?limit=100" -Headers @{
    "Authorization"     = "Bearer $token"
    "anthropic-version" = "2023-06-01"
    "anthropic-beta"    = "oauth-2025-04-20"
    "User-Agent"        = "claude-cli/2.1.196 (external, cli)"
}

$models = @()
foreach ($m in $resp.data) {
    $caps = $m.capabilities
    $models += [ordered]@{
        id                     = $m.id
        name                   = $m.display_name
        cost_per_1m_in         = 0
        cost_per_1m_out        = 0
        cost_per_1m_in_cached  = 0
        cost_per_1m_out_cached = 0
        context_window         = [int]$m.max_input_tokens
        default_max_tokens     = [int]$m.max_tokens
        can_reason             = [bool]$caps.thinking.supported
        supports_attachments   = [bool]$caps.image_input.supported
    }
}

if ($models.Count -eq 0) { throw "No models returned from /v1/models" }

$cfg = Get-Content -Raw $ConfigPath | ConvertFrom-Json
$cfg.providers.'claude-code'.models = $models
($cfg | ConvertTo-Json -Depth 20) | Set-Content -Encoding utf8 $ConfigPath

Write-Host "Wrote $($models.Count) models to $ConfigPath :"
$models | ForEach-Object { Write-Host ("  {0,-28} ctx={1} max={2}" -f $_.id, $_.context_window, $_.default_max_tokens) }
