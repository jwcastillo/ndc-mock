#!/usr/bin/env python3
"""Derive a version-translation mapping by diffing two IATA NDC schemas.

Element renames between generations cannot be guessed; they have to come from
the authoritative XSDs. This reads both, compares the declared element names,
and emits a mapping scaffold:

  - names in both            -> identical
  - names only in the source -> candidates for rename or drop
  - names only in the target -> the other side of those candidates

Likely renames are paired by normalised similarity and emitted as comments for a
human to confirm. Nothing is auto-accepted: a wrong rename is worse than a
missing one, because it produces a document that looks converted and is not.

Usage:
  derive-mapping.py <from.xsd|dir> <to.xsd|dir> --from-version 19.2 --to-version 21.3 \\
      [--out translations/19.2-to-21.3.json]

Get the schemas from https://airtechzone.iata.org/labs/tools/xsd-viewer.html
"""

from __future__ import annotations

import argparse
import difflib
import json
import re
import sys
from pathlib import Path

XSD_NS = "{http://www.w3.org/2001/XMLSchema}"
NAME_RE = re.compile(r'<xs:element[^>]*\bname="([^"]+)"', re.I)
TYPE_RE = re.compile(r'<xs:(?:complexType|simpleType)[^>]*\bname="([^"]+)"', re.I)
TARGET_NS_RE = re.compile(r'targetNamespace="([^"]+)"', re.I)


# IATA publishes rendered schema diagrams as SVG even where the raw XSD is
# gated. The diagram is generated from the schema, so its text nodes are an
# authoritative source of element names - enough to derive renames, though not
# the full type structure.
TEXT_RE = re.compile(r"<text[^>]*>([^<]+)</text>")
ELEMENT_NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]{1,}$")


def read_svg(path: Path) -> set[str]:
    text = path.read_text(encoding="utf-8", errors="replace")
    return {n for n in TEXT_RE.findall(text) if ELEMENT_NAME_RE.match(n.strip())}


def read_instance(path: Path) -> set[str]:
    """Element names actually present in an XML instance document."""
    text = path.read_text(encoding="utf-8", errors="replace")
    return set(re.findall(r"</?([A-Za-z_][A-Za-z0-9_.-]*)[\s/>]", text))


def read_schema(path: Path, elements_only: bool = False) -> tuple[set[str], set[str]]:
    """Return (element names, namespaces) declared under path.

    Accepts a directory of .xsd, a single .xsd, an .svg diagram, a .xml instance
    document, or a .txt list of names, so a version whose raw schema is gated can
    still be mapped.

    elements_only matters: an XSD declares both elements and named types, while
    an SVG diagram or an instance document shows only element names. Comparing a
    type inventory against an element inventory invents renames that are really
    just the two kinds of name seen side by side - and renaming a type name in a
    document does nothing, because type names never appear in instances.
    """
    if path.suffix.lower() == ".svg":
        return read_svg(path), set()
    if path.suffix.lower() == ".xml":
        return read_instance(path), set()
    if path.suffix.lower() == ".txt":
        names = {l.strip() for l in path.read_text().splitlines() if l.strip()}
        return names, set()
    files = sorted(path.glob("*.xsd")) if path.is_dir() else [path]
    if not files:
        sys.exit(f"no .xsd files under {path}")
    names: set[str] = set()
    namespaces: set[str] = set()
    for f in files:
        text = f.read_text(encoding="utf-8", errors="replace")
        names.update(NAME_RE.findall(text))
        if not elements_only:
            names.update(TYPE_RE.findall(text))
        namespaces.update(TARGET_NS_RE.findall(text))
    return names, namespaces


def normalise(name: str) -> str:
    """Strip the cosmetic differences IATA applies between generations."""
    n = re.sub(r"(Type|_Type)$", "", name)
    n = n.replace("_", "")
    return n.lower()


def pair_candidates(only_from: set[str], only_to: set[str]) -> list[tuple[str, str, float]]:
    """Pair leftover names by similarity, best match first, one use each."""
    by_norm_to: dict[str, list[str]] = {}
    for t in only_to:
        by_norm_to.setdefault(normalise(t), []).append(t)

    pairs: list[tuple[str, str, float]] = []
    taken: set[str] = set()

    # An exact match after normalisation is a rename with high confidence.
    for f in sorted(only_from):
        for t in by_norm_to.get(normalise(f), []):
            if t not in taken:
                pairs.append((f, t, 1.0))
                taken.add(t)
                break

    paired_from = {f for f, _, _ in pairs}
    remaining_to = sorted(only_to - taken)
    for f in sorted(only_from - paired_from):
        match = difflib.get_close_matches(f, remaining_to, n=1, cutoff=0.82)
        if match:
            pairs.append((f, match[0], difflib.SequenceMatcher(None, f, match[0]).ratio()))
            remaining_to.remove(match[0])
    return pairs


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("source")
    ap.add_argument("target")
    ap.add_argument("--from-version", required=True)
    ap.add_argument("--to-version", required=True)
    ap.add_argument("--out")
    ap.add_argument("--elements-only", action="store_true",
                    help="read only xs:element declarations from an XSD, ignoring named types. "
                         "Use this whenever the other side is an SVG diagram or an instance "
                         "document, so like is compared with like.")
    ap.add_argument("--confidence", type=float, default=1.0,
                    help="minimum similarity to place a pair in rename rather than in notes (default 1.0: exact-after-normalisation only)")
    args = ap.parse_args()

    from_names, from_ns = read_schema(Path(args.source), args.elements_only)
    to_names, to_ns = read_schema(Path(args.target), args.elements_only)

    identical = sorted(from_names & to_names)
    only_from = from_names - to_names
    only_to = to_names - from_names
    pairs = pair_candidates(only_from, only_to)

    rename = {f: t for f, t, score in pairs if score >= args.confidence}
    uncertain = [(f, t, round(score, 2)) for f, t, score in pairs if score < args.confidence]
    unpaired_from = sorted(only_from - set(rename) - {f for f, _, _ in pairs})

    namespaces = {}
    for f in sorted(from_ns):
        for t in sorted(to_ns):
            # Same message family, different generation segment.
            if f.rsplit("/", 1)[-1] == t.rsplit("/", 1)[-1]:
                namespaces[f] = t
    if not namespaces and from_ns and to_ns:
        namespaces = {sorted(from_ns)[0]: sorted(to_ns)[0]}

    notes = [
        f"derived from {args.source} -> {args.target}",
        f"{len(identical)} names identical, {len(rename)} renamed with confidence, "
        f"{len(uncertain)} uncertain, {len(unpaired_from)} source-only unresolved",
    ]
    if uncertain:
        notes.append("REVIEW uncertain pairs: " + ", ".join(f"{f}~{t}({s})" for f, t, s in uncertain[:20]))
    if unpaired_from:
        notes.append("source-only, decide rename or drop: " + ", ".join(unpaired_from[:20]))
    notes.append("structural changes are NOT expressible as renames and must be handled separately")

    out = {
        "from": args.from_version,
        "to": args.to_version,
        "namespaces": namespaces,
        "rename": rename,
        "drop": [],
        "identical": identical,
        "notes": notes,
    }

    text = json.dumps(out, indent=2, ensure_ascii=False) + "\n"
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
        print(f"wrote {args.out}", file=sys.stderr)
    else:
        print(text)

    print(
        f"identical={len(identical)} rename={len(rename)} uncertain={len(uncertain)} "
        f"unresolved={len(unpaired_from)} namespaces={len(namespaces)}",
        file=sys.stderr,
    )


if __name__ == "__main__":
    main()
