#!/usr/bin/env python3
"""Run oh-my-pi's `omp` on the same edit-benchmark fixtures with the same Haiku
model (via the Claude Code subscription), logging full JSON transcripts so we
can compare omp's tool usage and behavior against Crush's.

Uses the vendored fixtures under
crush/internal/agent/testdata/edit_evals/ (same corpus Crush is evaluated on).
Scoring is exact-match of the final file vs expected/.

Requires: the `omp` wrapper at /tmp/omp (bun 1.3.14 + published omp), and the
Claude subscription token bridged via ANTHROPIC_OAUTH_TOKEN.

Usage:
  scripts/omp-eval.py --limit 20 --concurrency 6 --out /tmp/omp_runs
  scripts/omp-eval.py --spread --out /tmp/omp_runs
"""
from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import subprocess
import sys
import tempfile
from collections import Counter
from pathlib import Path

CRUSH = Path("/home/coder/tools/crush")
FIXTURES = CRUSH / "internal" / "agent" / "testdata" / "edit_evals"
OMP = os.environ.get("OMP_BIN", "/tmp/omp")


def load_token() -> str:
    p = Path.home() / ".claude" / ".credentials.json"
    return json.loads(p.read_text())["claudeAiOauth"]["accessToken"]


def load_fixtures() -> list[dict]:
    out = []
    for d in sorted(FIXTURES.iterdir()):
        if not d.is_dir():
            continue
        inp = next((d / "input").iterdir())
        out.append({
            "name": d.name,
            "file": inp.name,
            "input": inp.read_text(),
            "expected": (d / "expected" / inp.name).read_text(),
            "prompt": (d / "prompt.md").read_text(),
        })
    return out


def category(name: str) -> str:
    return name.rsplit("-", 1)[0]


def run_one(fx: dict, model: str, token: str, out_dir: Path) -> dict:
    with tempfile.TemporaryDirectory(prefix="omp-eval-") as tmp:
        tmp = Path(tmp)
        (tmp / fx["file"]).write_text(fx["input"])
        cmd = [OMP, "-p", "--model", model, "--cwd", str(tmp),
               "--auto-approve", "--no-session", "--no-lsp", "--mode=json", fx["prompt"]]
        env = {**os.environ, "ANTHROPIC_OAUTH_TOKEN": token}
        try:
            p = subprocess.run(cmd, capture_output=True, text=True, timeout=240, env=env)
            transcript, stderr, rc = p.stdout, p.stderr, p.returncode
        except subprocess.TimeoutExpired:
            transcript, stderr, rc = "", "TIMEOUT", -1
        got = (tmp / fx["file"]).read_text()
        ok = got.replace("\r\n", "\n") == fx["expected"].replace("\r\n", "\n")

        # Persist the full transcript + result for later comparison.
        (out_dir).mkdir(parents=True, exist_ok=True)
        (out_dir / f"{fx['name']}.jsonl").write_text(transcript)
        tools = extract_tools(transcript)
        (out_dir / f"{fx['name']}.summary.json").write_text(json.dumps({
            "name": fx["name"], "ok": ok, "rc": rc, "tools": tools,
            "got": got, "expected": fx["expected"], "stderr": stderr[-2000:],
        }, indent=2))
        return {"name": fx["name"], "ok": ok, "rc": rc, "tools": tools}


def extract_tools(transcript: str) -> list[dict]:
    tools = []
    for ln in transcript.splitlines():
        try:
            o = json.loads(ln)
        except Exception:
            continue
        if o.get("type") == "tool_execution_start":
            tools.append({"tool": o.get("toolName"), "intent": o.get("intent")})
    return tools


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("-m", "--model", default="haiku")
    ap.add_argument("--only", default="")
    ap.add_argument("--spread", action="store_true")
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--concurrency", type=int, default=6)
    ap.add_argument("--out", default="/tmp/omp_runs")
    args = ap.parse_args()

    token = load_token()
    out_dir = Path(args.out)

    fixtures = load_fixtures()
    if args.spread:
        seen, sp = set(), []
        for fx in fixtures:
            c = category(fx["name"])
            if c not in seen:
                seen.add(c)
                sp.append(fx)
        fixtures = sp
    if args.only:
        subs = [s.strip() for s in args.only.split(",")]
        fixtures = [fx for fx in fixtures if any(s in fx["name"] for s in subs)]
    if args.limit:
        fixtures = fixtures[: args.limit]

    print(f"omp run: {len(fixtures)} fixtures, model {args.model}, "
          f"concurrency {args.concurrency}, transcripts -> {out_dir}", file=sys.stderr)

    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as ex:
        futs = [ex.submit(run_one, fx, args.model, token, out_dir) for fx in fixtures]
        for i, f in enumerate(concurrent.futures.as_completed(futs), 1):
            r = f.result()
            results.append(r)
            print(f"  [{i}/{len(fixtures)}] {r['name']:45s} {'PASS' if r['ok'] else 'FAIL'} "
                  f"tools={Counter(t['tool'] for t in r['tools'])}", file=sys.stderr)

    scoreboard(results, out_dir)
    return 0


def scoreboard(results: list[dict], out_dir: Path) -> None:
    total = len(results)
    passed = sum(r["ok"] for r in results)
    tool_freq = Counter()
    for r in results:
        tool_freq.update(t["tool"] for t in r["tools"])
    print("\n=== omp (Haiku) scoreboard ===")
    print(f"pass: {passed}/{total} ({100*passed/total:.1f}%)")
    print(f"tool usage (total calls): {dict(tool_freq)}")
    print(f"avg tool calls/task: {sum(tool_freq.values())/total:.2f}")
    fails = sorted(r["name"] for r in results if not r["ok"])
    if fails:
        print("failed: " + ", ".join(fails))
    (out_dir / "_scoreboard.json").write_text(json.dumps({
        "total": total, "passed": passed, "tool_freq": dict(tool_freq),
        "results": results,
    }, indent=2))


if __name__ == "__main__":
    sys.exit(main())
