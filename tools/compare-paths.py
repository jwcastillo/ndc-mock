#!/usr/bin/env python3
"""Compare element *paths* between a message XSD and IATA's schema diagram (SVG)
of a later generation, for every element name the later generation lacks.

derive-mapping.py compares names; a name alone cannot tell a rename from a move
or a removal. This prints each missing name's parents and what the later
generation has under the same parent, which is the evidence a rename or drop in
translations/*.json must rest on.

  tools/compare-paths.py 19.2/IATA_AirShoppingRS.xsd 24.1/IATA_AirShoppingRS.xsd
  tools/compare-paths.py 19.2/IATA_AirShoppingRS.xsd IATA_AirShoppingRS.svg

Either side can be an XSD or a diagram; prefer XSD on both when you have it.

The diagram's element ids encode the tree (_0_1_2 is a child of _0_1), so paths
are exact. A parent "expanded" in the diagram shows all its children; absence
under an expanded parent is removal, not a rendering shortcut.
"""
import re, sys, xml.etree.ElementTree as ET

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


def main(old_file, new_file):
    old, new = paths(old_file), paths(new_file)
    names = lambda ps: {p.rsplit('/', 1)[-1] for p in ps}
    new_names = names(new)
    children = lambda ps, par: {p[len(par) + 1:].split('/')[0] for p in ps if p.startswith(par + '/')}
    for name in sorted(names(old) - new_names):
        print(f'\n{name}')
        for p in sorted(q for q in old if q.endswith('/' + name))[:6]:
            par = p.rsplit('/', 1)[0]
            if par not in new:
                print(f'  {p}\n    parent gone in the later generation (moved or restructured)')
                continue
            added = sorted(children(new, par) - children(old, par))
            print(f'  {p}\n    parent kept; new there: {", ".join(added) or "nothing"}')

    # A name that survives elsewhere can still be gone from one parent, which is
    # what a scoped "Parent/Child" rename is for.
    print('\n--- name kept elsewhere, but no longer under this parent')
    for p in sorted(old - new):
        name, par = p.rsplit('/', 1)[-1], p.rsplit('/', 1)[0]
        if name in new_names and par in new:
            added = sorted(children(new, par) - children(old, par))
            print(f'  {p}\n    new there: {", ".join(added) or "nothing"}')


if __name__ == '__main__':
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(*sys.argv[1:])
