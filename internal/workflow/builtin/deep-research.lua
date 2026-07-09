-- deep-research: Scope -> pipeline(Search -> URL-dedup -> Fetch+Extract) -> 3-vote Verify -> Synthesize
--
-- Ported from a reverse-engineered "deep-research" workflow. Fans out
-- web searches across several angles, fetches and extracts falsifiable
-- claims from the results, adversarially verifies each claim with
-- independent voters, then synthesizes a cited report.
--
-- Question is passed via Workflow({name = "deep-research", args = "<question>"}).

local VOTES_PER_CLAIM = 3
local REFUTATIONS_REQUIRED = 2
local MAX_FETCH = 15
local MAX_VERIFY_CLAIMS = 25

-- === Schemas ===

local SCOPE_SCHEMA = {
  type = "object", required = { "question", "angles", "summary" },
  properties = {
    question = { type = "string" },
    summary = { type = "string" },
    angles = { type = "array", minItems = 3, maxItems = 6, items = {
      type = "object", required = { "label", "query" },
      properties = {
        label = { type = "string" },
        query = { type = "string" },
        rationale = { type = "string" },
      },
    } },
  },
}

local SEARCH_SCHEMA = {
  type = "object", required = { "results" },
  properties = {
    results = { type = "array", maxItems = 6, items = {
      type = "object", required = { "url", "title", "relevance" },
      properties = {
        url = { type = "string" },
        title = { type = "string" },
        snippet = { type = "string" },
        relevance = { enum = { "high", "medium", "low" } },
      },
    } },
  },
}

local EXTRACT_SCHEMA = {
  type = "object", required = { "claims", "sourceQuality" },
  properties = {
    sourceQuality = { enum = { "primary", "secondary", "blog", "forum", "unreliable" } },
    publishDate = { type = "string" },
    claims = { type = "array", maxItems = 5, items = {
      type = "object", required = { "claim", "quote", "importance" },
      properties = {
        claim = { type = "string" },
        quote = { type = "string" },
        importance = { enum = { "central", "supporting", "tangential" } },
      },
    } },
  },
}

local VERDICT_SCHEMA = {
  type = "object", required = { "refuted", "evidence", "confidence" },
  properties = {
    refuted = { type = "boolean" },
    evidence = { type = "string" },
    confidence = { enum = { "high", "medium", "low" } },
    counterSource = { type = "string" },
  },
}

local REPORT_SCHEMA = {
  type = "object", required = { "summary", "findings", "caveats" },
  properties = {
    summary = { type = "string" },
    findings = { type = "array", items = {
      type = "object", required = { "claim", "confidence", "sources", "evidence" },
      properties = {
        claim = { type = "string" },
        confidence = { enum = { "high", "medium", "low" } },
        sources = { type = "array", items = { type = "string" } },
        evidence = { type = "string" },
        vote = { type = "string" },
      },
    } },
    caveats = { type = "string" },
    openQuestions = { type = "array", items = { type = "string" } },
  },
}

-- === Phase 0: Scope -- decompose question into search angles ===
phase("Scope")

local QUESTION = args:match("^%s*(.-)%s*$")
if QUESTION == "" then
  return { error = "No research question provided. Pass it as args: Workflow({name = 'deep-research', args = '<question>'})." }
end

local scope = agent(
  "Decompose this research question into complementary search angles.\n\n" ..
  "## Question\n" .. QUESTION .. "\n\n" ..
  "## Task\n" ..
  "Generate 5 distinct web search queries that together cover the question from different angles. Pick angles that suit the question's domain. Examples:\n" ..
  "- broad/primary - academic/technical - recent news - contrarian/skeptical - practitioner/implementation\n" ..
  "- For medical: anatomy - common causes - serious differentials - authoritative refs - red flags\n" ..
  "- For tech: state-of-art - benchmarks - limitations - industry adoption - cost/tradeoffs\n\n" ..
  "Make queries specific enough to surface high-signal results. Avoid redundancy.\n" ..
  "Return: the question (verbatim or lightly normalized), a 1-2 sentence decomposition strategy, and the angles.\n\nStructured output only.",
  { label = "scope", schema = SCOPE_SCHEMA }
)
if not scope then
  return { error = "Scope agent returned no result -- cannot decompose the research question." }
end

log("Q: " .. QUESTION:sub(1, 80) .. (#QUESTION > 80 and "..." or ""))

local angleLabels = {}
for _, a in ipairs(scope.angles) do
  table.insert(angleLabels, a.label)
end
log("Decomposed into " .. #scope.angles .. " angles: " .. table.concat(angleLabels, ", "))

-- === Dedup state -- accumulates across searchers as they complete ===

local function normURL(u)
  local host, path = u:match("^%a+://([^/]+)(.*)$")
  if not host then
    return u:lower()
  end
  host = host:gsub("^www%.", "")
  path = path:gsub("/$", "")
  return (host .. path):lower()
end

local seen = {}
local dupes = {}
local budgetDropped = {}
local relRank = { high = 0, medium = 1, low = 2 }
local fetchSlots = MAX_FETCH

-- === Prompts ===

local function searchPrompt(angle)
  return "## Web Searcher: " .. angle.label .. "\n\n" ..
    "Research question: \"" .. QUESTION .. "\"\n\n" ..
    "Your angle: **" .. angle.label .. "** -- " .. (angle.rationale or "") .. "\n" ..
    "Search query: `" .. angle.query .. "`\n\n" ..
    "## Task\nUse the web_search tool with the query above (or a refined version). Return the top 4-6 most relevant results.\n" ..
    "Rank by relevance to the ORIGINAL question, not just the search query. Skip obvious SEO spam/content farms.\n" ..
    "Include a short snippet capturing why each result is relevant.\n\nStructured output only."
end

local function fetchPrompt(source, angle)
  return "## Source Extractor\n\n" ..
    "Research question: \"" .. QUESTION .. "\"\n\n" ..
    "Fetch and extract key claims from this source:\n" ..
    "**URL:** " .. source.url .. "\n**Title:** " .. source.title .. "\n**Found via:** " .. angle .. " search\n\n" ..
    "## Task\n1. Use the web_fetch tool to retrieve the page content.\n" ..
    "2. Assess source quality: primary research/institution? secondary reporting? blog/opinion? forum? unreliable?\n" ..
    "3. Extract 2-5 FALSIFIABLE claims that bear on the research question. Each claim must:\n" ..
    "   - be a concrete, checkable statement (not vague generalities)\n" ..
    "   - include a direct quote from the source as support\n" ..
    "   - be rated central/supporting/tangential to the research question\n" ..
    "4. Note publish date if available.\n\n" ..
    "If the fetch fails or the page is irrelevant/paywalled, return claims: [] and sourceQuality: \"unreliable\".\n\nStructured output only."
end

local function verifyPrompt(claim, v)
  return "## Adversarial Claim Verifier (voter " .. (v + 1) .. "/" .. VOTES_PER_CLAIM .. ")\n\n" ..
    "Be SKEPTICAL. Try to REFUTE this claim. >=" .. REFUTATIONS_REQUIRED .. "/" .. VOTES_PER_CLAIM .. " refutations kill it.\n\n" ..
    "## Research question\n" .. QUESTION .. "\n\n" ..
    "## Claim under review\n\"" .. claim.claim .. "\"\n\n" ..
    "**Source:** " .. claim.sourceUrl .. " (" .. claim.sourceQuality .. ")\n" ..
    "**Supporting quote:** \"" .. claim.quote .. "\"\n\n" ..
    "## Checklist\n" ..
    "1. Is the claim actually supported by the quote, or is it an overreach/misread?\n" ..
    "2. Search the web for contradicting evidence -- does any credible source dispute or heavily qualify this?\n" ..
    "3. Is the source quality sufficient for the claim's strength? (extraordinary claims need primary sources)\n" ..
    "4. Is the claim outdated? (check dates -- old claims about fast-moving fields are suspect)\n" ..
    "5. Is this a marketing claim / press release / cherry-picked benchmark / forum speculation?\n\n" ..
    "**refuted=true** if: unsupported by quote / contradicted / low-quality source for strong claim / outdated / marketing fluff.\n" ..
    "**refuted=false** ONLY if: claim is well-supported, current, and source quality matches claim strength.\n" ..
    "Default to refuted=true if uncertain.\n\nStructured output only. Evidence MUST be specific."
end

-- === Pipeline: search -> dedup -> fetch+extract (no barrier) ===
phase("Search")

local searchResults = pipeline(
  scope.angles,
  function(angle)
    -- Ranking/filtering raw search-tool results into structured output
    -- is mechanical work, not deep reasoning -- run it on the cheaper
    -- small model. Fetch/Verify stay on the default model: claim
    -- extraction and adversarial verification are what determine
    -- whether the final report is trustworthy, so their quality isn't
    -- worth trading for cost.
    local r = agent(searchPrompt(angle), { label = "search:" .. angle.label, phase = "Search", schema = SEARCH_SCHEMA, model = "small" })
    if not r then
      return false
    end
    log(angle.label .. ": " .. #r.results .. " results")
    return { angle = angle.label, results = r.results }
  end,
  function(searchResult)
    if not searchResult then
      return {}
    end

    local sorted = {}
    for _, r in ipairs(searchResult.results) do
      table.insert(sorted, r)
    end
    table.sort(sorted, function(a, b) return relRank[a.relevance] < relRank[b.relevance] end)

    local novel = {}
    for _, r in ipairs(sorted) do
      local key = normURL(r.url)
      if seen[key] then
        table.insert(dupes, { url = r.url, angle = searchResult.angle, dupOf = seen[key] })
      elseif fetchSlots <= 0 and relRank[r.relevance] >= 1 then
        table.insert(budgetDropped, { url = r.url, angle = searchResult.angle })
      else
        seen[key] = { angle = searchResult.angle, title = r.title }
        fetchSlots = fetchSlots - 1
        table.insert(novel, r)
      end
    end
    if #novel < #searchResult.results then
      log(searchResult.angle .. ": " .. #novel .. " novel (" .. (#searchResult.results - #novel) .. " filtered)")
    end

    phase("Fetch")
    local fetchFns = {}
    for _, source in ipairs(novel) do
      fetchFns[#fetchFns + 1] = function()
        local host = source.url:match("^%a+://([^/]+)") or "unknown"
        host = host:gsub("^www%.", "")
        local ext = agent(fetchPrompt(source, searchResult.angle), {
          label = "fetch:" .. host, phase = "Fetch", schema = EXTRACT_SCHEMA,
        })
        if not ext then
          log("fetch failed: " .. source.url)
          return { url = source.url, title = source.title, angle = searchResult.angle, sourceQuality = "unreliable", claims = {} }
        end
        local claims = {}
        for _, c in ipairs(ext.claims) do
          table.insert(claims, {
            claim = c.claim, quote = c.quote, importance = c.importance,
            sourceUrl = source.url, sourceQuality = ext.sourceQuality,
          })
        end
        return {
          url = source.url, title = source.title, angle = searchResult.angle,
          sourceQuality = ext.sourceQuality, publishDate = ext.publishDate, claims = claims,
        }
      end
    end
    local batch = parallel(fetchFns)
    return batch
  end
)

local allSources = {}
for _, batch in ipairs(searchResults) do
  if batch then
    for _, s in ipairs(batch) do
      if s then
        table.insert(allSources, s)
      end
    end
  end
end

local allClaims = {}
for _, s in ipairs(allSources) do
  for _, c in ipairs(s.claims) do
    table.insert(allClaims, c)
  end
end

local impRank = { central = 0, supporting = 1, tangential = 2 }
local qualRank = { primary = 0, secondary = 1, blog = 2, forum = 3, unreliable = 4 }

local rankedClaims = {}
for _, c in ipairs(allClaims) do
  table.insert(rankedClaims, c)
end
table.sort(rankedClaims, function(a, b)
  if impRank[a.importance] ~= impRank[b.importance] then
    return impRank[a.importance] < impRank[b.importance]
  end
  return qualRank[a.sourceQuality] < qualRank[b.sourceQuality]
end)
while #rankedClaims > MAX_VERIFY_CLAIMS do
  table.remove(rankedClaims)
end

log("Fetched " .. #allSources .. " sources -> " .. #allClaims .. " claims -> verifying top " .. #rankedClaims)

if #rankedClaims == 0 then
  local sources = {}
  for _, s in ipairs(allSources) do
    table.insert(sources, { url = s.url, quality = s.sourceQuality })
  end
  return {
    question = QUESTION,
    summary = "No claims extracted. " .. #allSources .. " sources fetched, all empty/failed. " ..
      #dupes .. " URL dupes, " .. #budgetDropped .. " budget-dropped.",
    findings = {}, refuted = {}, unverified = {}, sources = sources,
    stats = { angles = #scope.angles, sources = #allSources, claims = 0, dupes = #dupes },
  }
end

-- === Verify: 3-vote adversarial ===
phase("Verify")

local voteFns = {}
for _, claim in ipairs(rankedClaims) do
  voteFns[#voteFns + 1] = function()
    local voteBranches = {}
    for v = 0, VOTES_PER_CLAIM - 1 do
      voteBranches[#voteBranches + 1] = function()
        local v2 = agent(verifyPrompt(claim, v), {
          label = "v" .. v .. ":" .. claim.claim:sub(1, 40), phase = "Verify", schema = VERDICT_SCHEMA,
        })
        return v2
      end
    end
    local verdicts = parallel(voteBranches)

    local valid = {}
    for _, v in ipairs(verdicts) do
      if v then
        table.insert(valid, v)
      end
    end
    local refuted = 0
    for _, v in ipairs(valid) do
      if v.refuted then
        refuted = refuted + 1
      end
    end
    local errored = VOTES_PER_CLAIM - #valid
    local survives = #valid >= REFUTATIONS_REQUIRED and refuted < REFUTATIONS_REQUIRED
    local isRefuted = refuted >= REFUTATIONS_REQUIRED
    local mark = survives and "OK" or (isRefuted and "X" or "?")
    log("\"" .. claim.claim:sub(1, 50) .. "...\": " .. (#valid - refuted) .. "-" .. refuted ..
      (errored > 0 and (" (" .. errored .. " errored)") or "") .. " " .. mark)

    return {
      claim = claim.claim, quote = claim.quote, sourceUrl = claim.sourceUrl, sourceQuality = claim.sourceQuality,
      verdicts = valid, refutedVotes = refuted, erroredVotes = errored, survives = survives, isRefuted = isRefuted,
    }
  end
end

local voted = {}
for _, v in ipairs(parallel(voteFns)) do
  if v then
    table.insert(voted, v)
  end
end

local confirmed, killed, unverified = {}, {}, {}
for _, c in ipairs(voted) do
  if c.survives then
    table.insert(confirmed, c)
  elseif c.isRefuted then
    table.insert(killed, c)
  else
    table.insert(unverified, c)
  end
end

log("Verify done: " .. #voted .. " claims -> " .. #confirmed .. " confirmed, " .. #killed .. " refuted, " .. #unverified .. " unverified")

local function toRefuted(c)
  return { claim = c.claim, vote = (#c.verdicts - c.refutedVotes) .. "-" .. c.refutedVotes, source = c.sourceUrl }
end
local function toUnverified(c)
  return { claim = c.claim, erroredVotes = c.erroredVotes, validVotes = #c.verdicts, source = c.sourceUrl }
end

if #confirmed == 0 then
  -- Distinguish "refuted on merit" from "could not verify (infra error)".
  -- A run where every verifier failed is an infrastructure failure, not
  -- a research finding.
  local summary
  if #killed == 0 and #unverified > 0 then
    summary = "Could not verify any claims -- all " .. #unverified ..
      " verifier panels failed (likely rate-limiting or API errors). This is an infrastructure failure, not a research finding. Retry or verify manually."
  elseif #unverified > 0 then
    summary = #killed .. " claims refuted by adversarial verification; " .. #unverified ..
      " could not be verified (verifier agents failed). No claims survived. Research inconclusive."
  else
    summary = "All " .. #killed .. " claims refuted by adversarial verification. Research inconclusive -- sources may be low-quality or claims overstated."
  end

  local refutedOut, unverifiedOut, sourcesOut = {}, {}, {}
  for _, c in ipairs(killed) do table.insert(refutedOut, toRefuted(c)) end
  for _, c in ipairs(unverified) do table.insert(unverifiedOut, toUnverified(c)) end
  for _, s in ipairs(allSources) do table.insert(sourcesOut, { url = s.url, quality = s.sourceQuality, claimCount = #s.claims }) end

  return {
    question = QUESTION, summary = summary, findings = {},
    refuted = refutedOut, unverified = unverifiedOut, sources = sourcesOut,
    stats = {
      angles = #scope.angles, sources = #allSources, claims = #allClaims,
      verified = #voted, confirmed = 0, killed = #killed, unverified = #unverified,
    },
  }
end

-- === Synthesize ===
phase("Synthesize")

local confRank = { high = 0, medium = 1, low = 2 }
local blockParts = {}
for i, c in ipairs(confirmed) do
  local best = nil
  for _, v in ipairs(c.verdicts) do
    if not v.refuted and (not best or confRank[v.confidence] < confRank[best.confidence]) then
      best = v
    end
  end
  best = best or { confidence = "low", evidence = "" }
  table.insert(blockParts,
    "### [" .. (i - 1) .. "] " .. c.claim .. "\n" ..
    "Vote: " .. (#c.verdicts - c.refutedVotes) .. "-" .. c.refutedVotes .. " - Source: " .. c.sourceUrl .. " (" .. c.sourceQuality .. ")\n" ..
    "Quote: \"" .. c.quote .. "\"\nVerifier evidence (" .. best.confidence .. "): " .. best.evidence .. "\n")
end
local block = table.concat(blockParts, "\n")

local killedBlock = ""
if #killed > 0 then
  local parts = {}
  for _, c in ipairs(killed) do
    table.insert(parts, "- \"" .. c.claim .. "\" (" .. c.sourceUrl .. ", vote " .. (#c.verdicts - c.refutedVotes) .. "-" .. c.refutedVotes .. ")")
  end
  killedBlock = "\n## Refuted claims (for transparency)\n" .. table.concat(parts, "\n")
end

local unverifiedBlock = ""
if #unverified > 0 then
  local parts = {}
  for _, c in ipairs(unverified) do
    table.insert(parts, "- \"" .. c.claim .. "\" (" .. c.sourceUrl .. ", " .. c.erroredVotes .. "/" .. VOTES_PER_CLAIM .. " votes errored)")
  end
  unverifiedBlock = "\n## Unverified claims (" .. #unverified .. " -- verifier agents failed; neither confirmed nor refuted)\n" ..
    table.concat(parts, "\n") ..
    "\n\nMention in caveats that " .. #unverified .. " claim(s) could not be verified due to infrastructure errors."
end

local report = agent(
  "## Synthesis: research report\n\n" ..
  "**Question:** " .. QUESTION .. "\n\n" ..
  #confirmed .. " claims survived " .. VOTES_PER_CLAIM .. "-vote adversarial verification. Merge semantic duplicates and synthesize.\n\n" ..
  "## Confirmed claims\n" .. block .. "\n" .. killedBlock .. unverifiedBlock .. "\n\n" ..
  "## Instructions\n" ..
  "1. Identify claims that say the same thing -- merge them, combine their sources.\n" ..
  "2. Group related claims into coherent findings. Each finding should directly address the research question.\n" ..
  "3. Assign confidence per finding: high (multiple primary sources, unanimous votes), medium (secondary sources or split votes), low (single source or blog-quality).\n" ..
  "4. Write a 3-5 sentence executive summary answering the research question.\n" ..
  "5. Note caveats: what's uncertain, what sources were weak, what time-sensitivity applies.\n" ..
  "6. List 2-4 open questions that emerged but weren't answered.\n\nStructured output only.",
  { label = "synthesize", schema = REPORT_SCHEMA }
)

local refutedOut, unverifiedOut, sourcesOut = {}, {}, {}
for _, c in ipairs(killed) do table.insert(refutedOut, toRefuted(c)) end
for _, c in ipairs(unverified) do table.insert(unverifiedOut, toUnverified(c)) end
for _, s in ipairs(allSources) do
  table.insert(sourcesOut, { url = s.url, quality = s.sourceQuality, angle = s.angle, claimCount = #s.claims })
end

if not report then
  -- Synthesis skipped/errored -- salvage the verified claims raw rather
  -- than discarding the whole run.
  local confirmedOut = {}
  for _, c in ipairs(confirmed) do
    table.insert(confirmedOut, {
      claim = c.claim, source = c.sourceUrl, quote = c.quote,
      vote = (#c.verdicts - c.refutedVotes) .. "-" .. c.refutedVotes,
    })
  end
  return {
    question = QUESTION,
    summary = "Synthesis step was skipped or failed -- returning " .. #confirmed .. " verified claims unmerged.",
    findings = {}, confirmed = confirmedOut,
    refuted = refutedOut, unverified = unverifiedOut, sources = sourcesOut,
    stats = {
      angles = #scope.angles, sources = #allSources, claims = #allClaims, verified = #voted,
      confirmed = #confirmed, killed = #killed, unverified = #unverified, afterSynthesis = 0,
    },
  }
end

report.question = QUESTION
report.refuted = refutedOut
report.unverified = unverifiedOut
report.sources = sourcesOut
report.stats = {
  angles = #scope.angles,
  sourcesFetched = #allSources,
  claimsExtracted = #allClaims,
  claimsVerified = #voted,
  confirmed = #confirmed,
  killed = #killed,
  unverified = #unverified,
  afterSynthesis = #report.findings,
  urlDupes = #dupes,
  budgetDropped = #budgetDropped,
  agentCalls = 1 + #scope.angles + #allSources + (#voted * VOTES_PER_CLAIM) + 1,
}
return report
