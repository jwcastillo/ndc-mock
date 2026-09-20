#!/usr/bin/env python3
"""Compare element *paths* between a message XSD and IATA's schema diagram (SVG)
of a later generation, for every element name the later generation lacks.

derive-mapping.py compares names; a name alone cannot tell a rename from a move
or a removal. This prints each missing name's parents and what the later
generation has under the same parent, which is the evidence a rename or drop in
translations/*.json must rest on.

  tools/compare-paths.py 19.2/IATA_AirShoppingRS.xsd 24.1/IATA_AirShoppingRS.xsd
  tools/compare-paths.py 19.2/IATA_AirShoppingRS.xsd IATA_AirShoppingRS.svg

--typesafe annotates each unresolved path with what it thinks became of it, and
what it nearly thought instead. Wraps it settles in code; the rest is one TypeSafe
Choice over the elements that appeared under the same parent, with their subtrees,
which is evidence derive-mapping.py does not have when it compares bare names.

There is no threshold and nothing is auto-accepted. On the real schemas (19.2
IATA_AirShoppingRS.xsd against the 21.3 and 24.1 distributions) the readings agree
with the shipped mapping on 19 of the 22 paths it covers, on both pairs. Renames are
the reliable half: 4 of 4 each time, 0.71 to 0.97, and nothing the mapping drops was
ever read as a rename. The three misses are all a drop read as a restructure, because
the parent gained unrelated children - and no rule separates those from a rename,
since CharacteristicCode also disappears entirely from 21.3 and is one. Trust a
rename reading; check a restructure yourself. translations/*.json still rests on what
you confirm here.

Either side can be an XSD or a diagram; prefer XSD on both when you have it.

The diagram's element ids encode the tree (_0_1_2 is a child of _0_1), so paths
are exact. A parent "expanded" in the diagram shows all its children; absence
under an expanded parent is removal, not a rendering shortcut.
"""
import argparse, re, sys, xml.etree.ElementTree as ET

import typesafe_client

X = '{http://www.w3.org/2001/XMLSchema}'
local = lambda q: q.split(':')[-1] if q else q


def svg_paths(path):
    svg = open(path).read()
    els = {}
    for m in re.finditer(r'<g class="el[^"]*" id="([^"]+)"[^>]*>(.*?)</g>', svg, re.S):
        t = re.search(r'<text[^>]*>([^<]+)</text>', m.group(2))
        if t:
            els[m.group(1)] = t.group(1).strip()
    out = set()
    for i in els:
        parts, cur = [], i
        while cur:
            if cur in els:
                parts.append(els[cur])
            cur = cur[:cur.rfind('_')] if cur.count('_') > 1 else ''
        out.add('/'.join(reversed(parts)))
    return out


def xsd_paths(path):
    # From 21.3 the message schema imports its types from a common-types
    # schema; follow imports and includes so the whole tree is walked.
    import os
    types, elems, todo, done = {}, {}, [path], set()
    while todo:
        f = todo.pop()
        if f in done:
            continue
        done.add(f)
        r = ET.parse(f).getroot()
        for c in r:
            if c.tag in (X + 'import', X + 'include') and c.get('schemaLocation'):
                todo.append(os.path.join(os.path.dirname(f), c.get('schemaLocation')))
            elif c.tag in (X + 'complexType', X + 'simpleType'):
                types.setdefault(c.get('name'), c)
            elif c.tag == X + 'element' and f == path:
                elems[c.get('name')] = c
    out = set()

    def content(node, path, seen):
        # Descend through compositors and derivation, stopping at elements.
        for c in node:
            if c.tag == X + 'element':
                element(c, path, seen)
            elif c.tag in (X + 'extension', X + 'restriction'):
                b = local(c.get('base'))
                if b in types and b not in seen:
                    content(types[b], path, seen | {b})
                content(c, path, seen)
            elif c.tag in (X + 'sequence', X + 'choice', X + 'all', X + 'complexContent', X + 'complexType'):
                content(c, path, seen)

    def element(c, path, seen):
        name = c.get('name') or local(c.get('ref'))
        p = path + '/' + name if path else name
        out.add(p)
        decl = elems.get(name) if c.get('ref') else c
        t = local(decl.get('type')) if decl is not None else None
        if t in types and t not in seen:
            content(types[t], p, seen | {t})
        inline = decl.find(X + 'complexType') if decl is not None else None
        if inline is not None:
            content(inline, p, seen)

    for name, e in elems.items():
        element(e, '', set())
    return out


def paths(f):
    return svg_paths(f) if f.endswith('.svg') else xsd_paths(f)


RESTRUCTURED, REMOVED = "restructured", "removed"

QUESTION = (
    "These are two generations of the IATA NDC XML schemas. `source_path` exists in the source "
    "generation and no element of that name exists under `shared_parent` in the target. The named "
    "options are what the target has under `shared_parent` that the source did not. Where did the "
    "data `source_path` carried end up in the target?"
)

# The question states that the name is gone, so an option must not be recognisable by
# that alone: each one names a different fate for the DATA, which is what a mapping
# rule has to account for. An earlier wording defined "removed" as "the element is
# gone", and it drew a third to two thirds of the mass on paths whose replacement was
# sitting right there in the evidence.
RESTRUCTURED_DESC = (
    "The data is in the target, arranged differently: wrapped in a new level, split across "
    "elements, or folded into a sibling. No single new name translates it; it needs a "
    "structural rule."
)

# Deliberately NOT in that description: a lecture about spotting wrappers. wrapper_of()
# already settles every wrap the evidence proves, in code, for nothing. Telling the model
# to suspect one as well cost 4 out of 4 renames the shipped 19.2 -> 21.3 mapping has
# confirmed against the XSDs - it answered 3 of them correctly and at a confidence too
# low to act on. Leave the question to the case code cannot decide.
REMOVED_DESC = (
    "The data is not in the target at all. Not under any option offered, not anywhere else: "
    "the target simply does not carry it."
)


def subtree(paths, prefix, limit=12):
    """What the target holds under one candidate, which is how a wrap gives itself away."""
    under = sorted(q[len(prefix) + 1:] for q in paths if q.startswith(prefix + '/'))
    return under[:limit] + ([f'... {len(under) - limit} more'] if len(under) > limit else [])


def question_for(p, old, new, added):
    par = p.rsplit('/', 1)[0]
    criteria = {c: {"target_path": f"{par}/{c}",
                    "contains": subtree(new, f"{par}/{c}") or "nothing",
                    "choose_when": f"{c} is `source_path` under a new name, carrying the same "
                                   "data in the same role."}
                for c in added}
    criteria[RESTRUCTURED] = RESTRUCTURED_DESC
    criteria[REMOVED] = REMOVED_DESC
    return {
        "type": "choice",
        "instructions": {
            "source_path": p,
            "source_element_contains": subtree(old, p) or "nothing",
            "shared_parent": par,
            "question": QUESTION,
        },
        "criteria": criteria,
    }


ALL = 10 ** 6


def wrapper_of(p, old, new, added):
    """The option that is a new level around `p` rather than `p`'s new name, if any.

    Two shapes prove it without asking anyone. The name that is supposedly gone is
    sitting inside the candidate; or the candidate has exactly one child and that
    child carries `p`'s subtree unchanged, which is a wrap whose inner element was
    renamed too. Both are set comparisons, so they are code's to make, not a model's.
    """
    par, name = p.rsplit('/', 1)[0], p.rsplit('/', 1)[-1]
    source = set(subtree(old, p, ALL))
    for c in added:
        under = subtree(new, f'{par}/{c}', ALL)
        if any(q.split('/')[0] == name for q in under):
            return c
        kids = {q.split('/')[0] for q in under}
        if source and len(kids) == 1:
            only = kids.pop()
            if {q[len(only) + 1:] for q in under if q.startswith(only + '/')} == source:
                return c
    return None


def judge(items, old, new, model):
    """One Choice per unresolved path. Items the evidence already settles cost nothing.

    No threshold, and no routing: this tool is something you read. Confidence cannot
    separate a rename from a restructure here, because with a wrap both are partly
    true - measured on the four renames the shipped 19.2 -> 21.3 mapping confirms
    against the XSDs, the model names the right element 4 times out of 4 at
    confidences (0.27 to 0.67) that overlap the cases whose real answer is
    "restructured" (0.09 to 0.46). So it prints what it thinks and what came second,
    and you decide. The one call it makes on its own is the wrap, which is arithmetic.
    """
    settled, askable = {}, {}
    for p, added in items.items():
        wrap = wrapper_of(p, old, new, added)
        if wrap:
            settled[p] = f'restructured: wrapped in {wrap}'
        elif added:
            askable[p] = added
        else:
            settled[p] = 'removed: nothing new appeared under this parent'
    if not askable:
        return settled
    state = {"domain": "IATA NDC XML message schemas",
             "source_generation": "the earlier schema, which every source_path comes from",
             "target_generation": "the later schema, which every option comes from"}
    answers = typesafe_client.ask(
        state, {p: question_for(p, old, new, added) for p, added in askable.items()}, model)
    return settled | {p: top_two(answers[p]) for p in askable}


def top_two(answer):
    """What it thinks, and what it nearly thought instead - the contest is the signal."""
    ranked = sorted(answer["probabilities"].items(), key=lambda kv: -kv[1])
    say = lambda name, prob: (name if name in (RESTRUCTURED, REMOVED) else f'renamed to {name}') \
        + f' {prob:.2f}'
    best = say(*ranked[0])
    return f'{best}, then {say(*ranked[1])}' if len(ranked) > 1 and ranked[1][1] >= 0.15 else best


def line(p, verdicts):
    """The reading, or nothing at all when --typesafe was not asked for."""
    return f'    -> {verdicts[p]}' if p in verdicts else ''


def main(old_file, new_file, use_typesafe=False, model=typesafe_client.DEFAULT_MODEL):
    old, new = paths(old_file), paths(new_file)
    names = lambda ps: {p.rsplit('/', 1)[-1] for p in ps}
    new_names = names(new)
    children = lambda ps, par: {p[len(par) + 1:].split('/')[0] for p in ps if p.startswith(par + '/')}

    # Collect first, ask once: every question is about the same pair of schemas, so
    # they are independent and ride together instead of one request per name.
    gone = sorted(names(old) - new_names)
    unresolved = {}
    for name in gone:
        for p in sorted(q for q in old if q.endswith('/' + name))[:6]:
            par = p.rsplit('/', 1)[0]
            if par in new:
                unresolved[p] = sorted(children(new, par) - children(old, par))
    scoped = [p for p in sorted(old - new)
              if p.rsplit('/', 1)[-1] in new_names and p.rsplit('/', 1)[0] in new]
    for p in scoped:
        unresolved.setdefault(p, sorted(children(new, p.rsplit('/', 1)[0]) - children(old, p.rsplit('/', 1)[0])))

    verdicts = judge(unresolved, old, new, model) if use_typesafe else {}

    for name in gone:
        print(f'\n{name}')
        for p in sorted(q for q in old if q.endswith('/' + name))[:6]:
            par = p.rsplit('/', 1)[0]
            if par not in new:
                print(f'  {p}\n    parent gone in the later generation (moved or restructured)')
                continue
            added = sorted(children(new, par) - children(old, par))
            print(f'  {p}\n    parent kept; new there: {", ".join(added) or "nothing"}')
            print(line(p, verdicts)) if line(p, verdicts) else None

    # A name that survives elsewhere can still be gone from one parent, which is
    # what a scoped "Parent/Child" rename is for.
    print('\n--- name kept elsewhere, but no longer under this parent')
    for p in scoped:
        par = p.rsplit('/', 1)[0]
        added = sorted(children(new, par) - children(old, par))
        print(f'  {p}\n    new there: {", ".join(added) or "nothing"}')
        print(line(p, verdicts)) if line(p, verdicts) else None

    if verdicts:
        print(f'\n--- {model} read {len(verdicts)} unresolved paths. Every line above is a '
              'reading, not a rule:\nconfirm it here, then write it into translations/*.json '
              'and let validate-translation.sh settle it.', file=sys.stderr)


def self_check():
    """What code decides on its own, checked without a network or a key.

    The four NOT-a-wrap shapes are the renames translations/19.2-to-21.3.json has
    confirmed against the XSDs. If wrapper_of ever swallows one, the model never
    gets asked and a real rename is reported as a restructure.
    """
    w = lambda old, new, p: wrapper_of(p, old, new, sorted(
        {q[len(p.rsplit('/', 1)[0]) + 1:].split('/')[0] for q in new
         if q.startswith(p.rsplit('/', 1)[0] + '/')}
        - {q[len(p.rsplit('/', 1)[0]) + 1:].split('/')[0] for q in old
           if q.startswith(p.rsplit('/', 1)[0] + '/')}))

    # the name reappears inside the candidate
    assert w({'A', 'A/X', 'A/X/K'}, {'A', 'A/W', 'A/W/X', 'A/W/X/K'}, 'A/X') == 'W'
    # one child, carrying the same subtree, renamed along the way
    assert w({'A', 'A/X', 'A/X/K', 'A/X/K/L'},
             {'A', 'A/W', 'A/W/Y', 'A/W/Y/K', 'A/W/Y/K/L'}, 'A/X') == 'W'
    # a leaf with no subtree cannot prove a wrap either way
    assert w({'A', 'A/X'}, {'A', 'A/Y'}, 'A/X') is None

    for old, new, path in (
        ({'S', 'S/CharacteristicCode', 'S/Code'}, {'S', 'S/SeatCharacteristicCode', 'S/Code'},
         'S/CharacteristicCode'),
        ({'B', 'B/BaggageFlightAssociations', 'B/BaggageFlightAssociations/PaxSegmentRefID'},
         {'B', 'B/OfferFlightAssociations', 'B/OfferFlightAssociations/PaxSegmentRefID'},
         'B/BaggageFlightAssociations'),
        ({'L', 'L/ALaCarteOfferItem', 'L/ALaCarteOfferItem/OfferItemID', 'L/ALaCarteOfferItem/Eligibility'},
         {'L', 'L/OfferItem', 'L/OfferItem/OfferItemID', 'L/OfferItem/Eligibility'},
         'L/ALaCarteOfferItem'),
        ({'R', 'R/AddOfferItem', 'R/AddOfferItem/OfferItemID'},
         {'R', 'R/AddedOfferItem', 'R/AddedOfferItem/OfferItemID'}, 'R/AddOfferItem'),
    ):
        assert w(old, new, path) is None, f'{path}: a confirmed rename read as a wrap'

    assert top_two({'probabilities': {'N': 0.9, RESTRUCTURED: 0.1}}) == 'renamed to N 0.90'
    assert top_two({'probabilities': {'N': 0.5, RESTRUCTURED: 0.4}}) \
        == 'renamed to N 0.50, then restructured 0.40'
    print('self-check ok')


if __name__ == '__main__':
    if '--self-check' in sys.argv:   # no schemas needed, no network
        self_check()
        sys.exit()
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('old')
    ap.add_argument('new')
    ap.add_argument('--typesafe', action='store_true',
                    help='say what became of each unresolved path, from the path evidence above. '
                         'Needs TYPESAFE_API_KEY. Writes no mapping rule.')
    ap.add_argument('--typesafe-model', default=typesafe_client.DEFAULT_MODEL)
    a = ap.parse_args()
    main(a.old, a.new, a.typesafe, a.typesafe_model)
