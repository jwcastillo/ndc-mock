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

--typesafe puts the leftovers in a useful order instead of an alphabetical one: one
TypeSafe Choice per unresolved name, over the target names string similarity found
closest plus a none-of-these option, routed by the answer's own confidence into
proposed rename / proposed drop / too uncertain. It writes nothing into the mapping.
Needs TYPESAFE_API_KEY.

Usage:
  derive-mapping.py <from.xsd|dir> <to.xsd|dir> --from-version 19.2 --to-version 21.3 \\
      [--out translations/19.2-to-21.3.json] [--typesafe]

Get the schemas from https://airtechzone.iata.org/labs/tools/xsd-viewer.html
"""

from __future__ import annotations

import argparse
import difflib
import json
import os
import re
import sys
import urllib.error
import urllib.request
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


TYPESAFE_URL = "https://api.typesafe.ai/v1/systemone"
SHORTLIST = 8          # candidate target names offered per source name
QUESTIONS_PER_CALL = 32
NO_MATCH = "none_of_these"

RENAME_QUESTION = (
    "IATA renames some elements between NDC schema generations. Which of the target-schema "
    "element names offered is `source_element` under its new name: the same element, carrying "
    "the same data, in the same role? Answer " + NO_MATCH + " if `source_element` was removed, "
    "split, absorbed into a different structure, or if the offered names are merely "
    "similar-looking elements that mean something else."
)


def shortlist(name: str, targets: list[str], n: int = SHORTLIST) -> list[str]:
    """The n most string-similar target names. A name not offered cannot be chosen."""
    ranked = sorted(targets, key=lambda t: difflib.SequenceMatcher(None, name, t).ratio(), reverse=True)
    return ranked[:n]


def route(choice: str, confidence: float, threshold: float) -> str:
    """Confidence decides whether to act on the answer, the answer decides what."""
    if confidence < threshold:
        return "human"
    return "drop" if choice == NO_MATCH else "rename"


def post(payload: dict, api_key: str) -> dict:
    """The key lives in this header and nowhere else: not in argv, not in the output."""
    req = urllib.request.Request(
        TYPESAFE_URL,
        data=json.dumps(payload).encode(),
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        # Report the status, never the request: a traceback here would carry the header.
        sys.exit(f"TypeSafe returned {e.code} {e.reason}")
    except urllib.error.URLError as e:
        sys.exit(f"TypeSafe unreachable: {e.reason}")


def triage(names: list[str], targets: list[str], from_v: str, to_v: str,
           model: str, threshold: float) -> list[dict]:
    """Ask one Choice per unresolved source name; route each answer by its confidence.

    The model orders the review queue; it does not write the mapping. A rename it
    proposes still has to hold up on paths (compare-paths.py) and then validate.
    """
    api_key = os.environ.get("TYPESAFE_API_KEY")
    if not api_key:
        sys.exit("TYPESAFE_API_KEY is not set. Export it from a gitignored *.env, the way\n"
                 "the provider credentials are handled; get a key at https://console.typesafe.ai/")
    if not targets:
        return [{"source": n, "route": "drop", "choice": NO_MATCH, "confidence": 1.0,
                 "why": "the target inventory has no unmatched name left to rename to"}
                for n in names]

    state = {
        "domain": "IATA NDC XML message schemas",
        "source_version": from_v,
        "target_version": to_v,
    }
    out: list[dict] = []
    for i in range(0, len(names), QUESTIONS_PER_CALL):
        chunk = names[i:i + QUESTIONS_PER_CALL]
        options = {n: shortlist(n, targets) for n in chunk}
        questions = {
            f"q{j}": {
                "type": "choice",
                "instructions": {"source_element": n, "question": RENAME_QUESTION},
                "criteria": dict.fromkeys(options[n])
                | {NO_MATCH: "`source_element` has no counterpart among the offered names."},
            }
            for j, n in enumerate(chunk)
        }
        answers = post({"state": state, "model": model, "questions": questions}, api_key)["answers"]
        for j, n in enumerate(chunk):
            a = answers[f"q{j}"]
            out.append({
                "source": n,
                "choice": a["choice"],
                "confidence": round(a["confidence"], 3),
                "probability": round(a["probabilities"][a["choice"]], 3),
                "route": route(a["choice"], a["confidence"], threshold),
                "considered": options[n],
            })
    return out


def self_check() -> None:
    # The shortlist only has to contain the right answer; it cannot recognise it.
    # Here string similarity puts ServiceOrder (0.83) above RepriceOrderRequest (0.77),
    # which is backwards - both are offered and the model decides.
    assert set(shortlist("RepriceOrder", ["ServiceOrder", "Baggage", "RepriceOrderRequest"], 2)) \
        == {"RepriceOrderRequest", "ServiceOrder"}, "the unrelated name must be cut, both plausible ones kept"
    assert shortlist("X", [], 3) == []
    assert route("ServiceOrder", 0.9, 0.8) == "rename"
    assert route(NO_MATCH, 0.9, 0.8) == "drop"
    assert route("ServiceOrder", 0.7, 0.8) == "human", "below threshold never auto-decides"
    assert route(NO_MATCH, 0.7, 0.8) == "human"
    print("self-check ok")


def main() -> None:
    if "--self-check" in sys.argv:   # no schemas needed, no network
        self_check()
        return
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
    ap.add_argument("--typesafe", action="store_true",
                    help="triage the names string similarity could not settle by asking a TypeSafe "
                         "Choice per name, and emit them as an ordered review queue. Needs "
                         "TYPESAFE_API_KEY. Still writes nothing into rename: a proposal is a "
                         "reading order for the reviewer, not a derivation from the schema.")
    ap.add_argument("--typesafe-model", default="jev-latest")
    ap.add_argument("--typesafe-confidence", type=float, default=0.8,
                    help="below this the answer is not acted on and the name goes to a human "
                         "(default 0.8; evaluate it on your own schema pairs)")
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

    unresolved = [f for f, _, _ in pairs if f not in rename] + unpaired_from
    reviewed = []
    if args.typesafe and unresolved:
        remaining_to = sorted(only_to - set(rename.values()))
        reviewed = triage(unresolved, remaining_to, args.from_version, args.to_version,
                          args.typesafe_model, args.typesafe_confidence)
        reviewed.sort(key=lambda r: (r["route"] != "rename", -r["confidence"]))

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
    if reviewed:
        counts = {r: sum(1 for x in reviewed if x["route"] == r) for r in ("rename", "drop", "human")}
        notes.append(
            f"REVIEW QUEUE ({args.typesafe_model}, act above confidence {args.typesafe_confidence}): "
            f"{counts['rename']} proposed renames, {counts['drop']} proposed drops, "
            f"{counts['human']} too uncertain to route. Proposals are NOT part of the mapping: "
            "confirm each on paths with compare-paths.py, then move it into rename or drop by hand."
        )
        out["review"] = reviewed

    text = json.dumps(out, indent=2, ensure_ascii=False) + "\n"
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
        print(f"wrote {args.out}", file=sys.stderr)
    else:
        print(text)

    print(
        f"identical={len(identical)} rename={len(rename)} uncertain={len(uncertain)} "
        f"unresolved={len(unpaired_from)} namespaces={len(namespaces)}"
        + (f" reviewed={len(reviewed)}" if reviewed else ""),
        file=sys.stderr,
    )


if __name__ == "__main__":
    main()
