#!/usr/bin/env python3
"""Trim ~/scaleplex-corpus to one entry per (shape + normalized argv) pair.

All entries with capture_source == "plex_wrapper_nfs" are preserved
unconditionally — they're pristine PMS argv from production usage and
the corpus we're trying to grow. Worker-side captures (worker_nfs_json,
worker_log) are deduped by hashing the argv after replacing volatile
tokens (file paths, UUIDs, X-Plex-Token) with placeholders.

Usage:
    python3 dedupe.py [--dry-run] [--output-dir ~/scaleplex-corpus-dedup]

Default: copies kept entries into ~/scaleplex-corpus-dedup/ (does NOT
mutate the source dir).
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import shutil
from collections import Counter
from pathlib import Path

VOLATILE = re.compile(
    r"/media/[^\s\"]+"  # media file paths
    r"|/transcode/Transcode/Sessions/[A-Za-z0-9_-]+"  # session dirs
    r"|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"  # UUIDs
    r"|X-Plex-Token=[A-Za-z0-9_-]+"  # tokens
)


def normalize_argv(argv: list[str]) -> str:
    return "\n".join(VOLATILE.sub("<X>", a) for a in argv)


def shape_key(d: dict) -> tuple:
    return (
        d.get("output_format"),
        d.get("output_codec"),
        d.get("has_input_seek"),
        d.get("has_map_inlineass"),
        d.get("input_count"),
    )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--corpus",
        default=str(Path("~/scaleplex-corpus").expanduser()),
        help="source corpus dir",
    )
    parser.add_argument(
        "--output-dir",
        default=str(Path("~/scaleplex-corpus-dedup").expanduser()),
        help="destination dir for kept entries",
    )
    parser.add_argument("--dry-run", action="store_true", help="report only, no copy")
    parser.add_argument(
        "--max-per-shape",
        type=int,
        default=0,
        help="cap worker entries per shape bucket (0 = no cap; only argv-hash dedup)",
    )
    args = parser.parse_args()

    src = Path(args.corpus)
    dst = Path(args.output_dir)
    if not src.is_dir():
        print(f"corpus dir does not exist: {src}")
        return 1

    files = sorted(src.glob("*.json"))
    print(f"scanning {len(files)} entries in {src}")

    seen_hash: dict[str, str] = {}
    kept: list[Path] = []
    kept_shape: Counter[tuple] = Counter()
    skipped_dup = 0
    skipped_bad = 0
    kept_wrapper = 0

    for p in files:
        try:
            with p.open() as f:
                d = json.load(f)
        except Exception:
            skipped_bad += 1
            continue
        if d.get("capture_source") == "plex_wrapper_nfs":
            kept.append(p)
            kept_shape[shape_key(d)] += 1
            kept_wrapper += 1
            continue
        argv = d.get("argv") or []
        if not argv:
            skipped_bad += 1
            continue
        norm = normalize_argv(argv)
        h = hashlib.sha256(norm.encode()).hexdigest()[:16]
        if h in seen_hash:
            skipped_dup += 1
            continue
        sk = shape_key(d)
        if args.max_per_shape > 0 and kept_shape[sk] >= args.max_per_shape:
            skipped_dup += 1
            continue
        seen_hash[h] = p.name
        kept.append(p)
        kept_shape[sk] += 1

    print(f"kept: {len(kept)} ({kept_wrapper} plex_wrapper preserved)")
    print(f"dropped duplicates: {skipped_dup}")
    print(f"dropped bad/empty: {skipped_bad}")
    print()
    print("shape distribution (kept):")
    for key, n in kept_shape.most_common():
        fmt, codec, seek, sub, ic = key
        print(f"  {n:4}  fmt={fmt} codec={codec} seek={seek} sub={sub} input_count={ic}")

    if args.dry_run:
        return 0

    dst.mkdir(parents=True, exist_ok=True)
    for p in kept:
        target = dst / p.name
        if not target.exists():
            shutil.copy2(p, target)
    print(f"\nwrote {len(kept)} files to {dst}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
