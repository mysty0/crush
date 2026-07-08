#!/usr/bin/env python3
"""Edit-tool eval harness — drives the real `crush run` binary (no Go tests).

For each fixture x edit-mode it: creates a temp project with the fixture's
input file + a crush.json toggling options.edit_mode, runs `crush run` with the
fixture prompt (non-interactive runs auto-approve permissions), then compares
the resulting file to the fixture's expected output.

Scoring is exact-match. Fixtures come from the vendored oh-my-pi
typescript-edit-benchmark corpus under internal/agent/testdata/edit_evals/.

Usage:
  scripts/edit-eval.py --build                 # build the binary first
  scripts/edit-eval.py -m claude-code/claude-haiku-4-5-20251001
  scripts/edit-eval.py --modes hashline --only operator-remove-negation --repeat 5 --dump /tmp/d
  scripts/edit-eval.py --spread                # one fixture per category

Env: uses your configured providers (e.g. the claude-code subscription).
"""
from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import shutil
import subprocess
import sys
import tempfile
from collections import defaultdict
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
FIXTURES = REPO / "internal" / "agent" / "testdata" / "edit_evals"
BINARY = Path(os.environ.get("CRUSH_BIN", "/tmp/crush-eval"))


def build_binary() -> None:
    print(f"building {BINARY} ...", file=sys.stderr)
    env = {**os.environ, "CGO_ENABLED": "0", "GOEXPERIMENT": "greenteagc"}
    subprocess.run(["go", "build", "-o", str(BINARY), "."], cwd=REPO, env=env, check=True)


def load_fixtures() -> list[dict]:
    out = []
    for d in sorted(FIXTURES.iterdir()):
        if not d.is_dir():
            continue
        inp = next((d / "input").iterdir())
        meta = json.loads((d / "metadata.json").read_text()) if (d / "metadata.json").exists() else {}
        out.append({
            "name": d.name,
            "file": inp.name,
            "input": inp.read_text(),
            "expected": (d / "expected" / inp.name).read_text(),
            "prompt": (d / "prompt.md").read_text(),
            "meta": meta,
        })
    return out


def category(name: str) -> str:
    return name.rsplit("-", 1)[0]


def run_one(fx: dict, mode: str, model: str, dump: str | None) -> dict:
    with tempfile.TemporaryDirectory(prefix="edit-eval-") as tmp:
        tmp = Path(tmp)
        (tmp / fx["file"]).write_text(fx["input"])
        (tmp / "crush.json").write_text(json.dumps({
            "options": {"edit_mode": mode, "auto_lsp": False, "disable_provider_auto_update": True},
        }))
        cmd = [str(BINARY), "run", "-q", "-c", str(tmp), "-m", model, fx["prompt"]]
        try:
            p = subprocess.run(cmd, capture_output=True, text=True, timeout=180)
            stdout, stderr, rc = p.stdout, p.stderr, p.returncode
        except subprocess.TimeoutExpired:
            stdout, stderr, rc = "", "TIMEOUT", -1
        got = (tmp / fx["file"]).read_text()
        ok = got.replace("\r\n", "\n") == fx["expected"].replace("\r\n", "\n")
        if dump and not ok:
            Path(dump).mkdir(parents=True, exist_ok=True)
            (Path(dump) / f"{mode}__{fx['name']}.txt").write_text(
                f"# {mode} / {fx['name']} FAIL (rc={rc})\n\n## PROMPT\n{fx['prompt']}\n"
                f"\n## STDOUT\n{stdout}\n\n## STDERR\n{stderr}\n"
                f"\n## EXPECTED\n{fx['expected']}\n\n## GOT\n{got}\n"
            )
        return {"name": fx["name"], "mode": mode, "ok": ok, "rc": rc}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--build", action="store_true")
    ap.add_argument("-m", "--model", default="claude-code/claude-haiku-4-5-20251001")
    ap.add_argument("--modes", default="string,hashline")
    ap.add_argument("--only", default="", help="comma substrings to filter fixtures")
    ap.add_argument("--spread", action="store_true", help="one fixture per category")
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--repeat", type=int, default=1)
    ap.add_argument("--concurrency", type=int, default=6)
    ap.add_argument("--dump", default="")
    args = ap.parse_args()

    if args.build or not BINARY.exists():
        build_binary()

    fixtures = load_fixtures()
    if args.spread:
        seen, spread = set(), []
        for fx in fixtures:
            c = category(fx["name"])
            if c not in seen:
                seen.add(c)
                spread.append(fx)
        fixtures = spread
    if args.only:
        subs = [s.strip() for s in args.only.split(",")]
        fixtures = [fx for fx in fixtures if any(s in fx["name"] for s in subs)]
    if args.limit:
        fixtures = fixtures[: args.limit]

    modes = args.modes.split(",")
    jobs = [(fx, mode) for mode in modes for fx in fixtures for _ in range(args.repeat)]
    print(f"{len(jobs)} runs — {len(fixtures)} fixtures x {len(modes)} modes x {args.repeat} "
          f"— model {args.model} — concurrency {args.concurrency}", file=sys.stderr)

    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as ex:
        futs = [ex.submit(run_one, fx, mode, args.model, args.dump or None) for fx, mode in jobs]
        for i, f in enumerate(concurrent.futures.as_completed(futs), 1):
            r = f.result()
            results.append(r)
            print(f"  [{i}/{len(jobs)}] {r['mode']:9s} {r['name']:45s} {'PASS' if r['ok'] else 'FAIL'}",
                  file=sys.stderr)

    scoreboard(results, fixtures, modes)
    return 0


def scoreboard(results: list[dict], fixtures: list[dict], modes: list[str]) -> None:
    meta_by = {fx["name"]: fx["meta"] for fx in fixtures}
    agg = {m: {"total": 0, "pass": 0} for m in modes}
    rep = {m: {"total": 0, "pass": 0} for m in modes}  # repeated-line fixtures only
    fails = defaultdict(list)
    for r in results:
        a = agg[r["mode"]]
        a["total"] += 1
        a["pass"] += r["ok"]
        if meta_by.get(r["name"], {}).get("is_repeated_line"):
            rr = rep[r["mode"]]
            rr["total"] += 1
            rr["pass"] += r["ok"]
        if not r["ok"]:
            fails[r["mode"]].append(r["name"])

    print("\n=== Edit-tool eval scoreboard ===")
    print(f"{'mode':10s} {'pass':>10s} {'pass%':>8s} {'repeated-line pass':>22s}")
    for m in modes:
        a, rr = agg[m], rep[m]
        pct = 100 * a["pass"] / a["total"] if a["total"] else 0
        rpct = 100 * rr["pass"] / rr["total"] if rr["total"] else 0
        print(f"{m:10s} {a['pass']:>4d}/{a['total']:<5d} {pct:>7.1f}% "
              f"{rr['pass']:>6d}/{rr['total']:<4d} ({rpct:.0f}%)")
    for m in modes:
        if fails[m]:
            print(f"\n{m} failed: {', '.join(sorted(fails[m]))}")


if __name__ == "__main__":
    sys.exit(main())
