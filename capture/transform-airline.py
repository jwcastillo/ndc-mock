#!/usr/bin/env python3
"""Rewrite a captured response as a different carrier.

Preserves the fare DISTRIBUTION of the capture (real pricing logic) and adjusts:
  - carrier code in the offer identifiers and the body
  - currency, via fxToUSD, keeping economic value
  - magnitude, via the carrier's fareScale

Does not synthesise live inventory: routes and fares come out plausible for the
carrier, not that carrier's real availability on a given date.

Usage: transform-airline.py <capture.xml> <CARRIER_CODE> > out.xml
"""
import base64, json, re, sys, os

root=os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
cfg=json.load(open(os.path.join(root,'airlines.json')))
src, code = sys.argv[1], sys.argv[2]
A=cfg['airlines'][code]; fx=cfg['fxToUSD']
dst_cur=A['currency']; scale=A['fareScale']

def convert_price(cur, amount):
    usd=amount*fx.get(cur,1.0)*scale
    return dst_cur, round(usd/fx.get(dst_cur,1.0))

def recode_offeritemid(m):
    out=[]
    for s in m.group(1).split('|'):
        try:
            d=base64.b64decode(s).decode('utf-8')
            if not d.isprintable(): raise ValueError
            d=re.sub(r'(?<![A-Z])'+ref+r'(?![A-Z])', code, d)   # carrier
            pm=re.match(r'PR=([A-Z]{3})/(\d+)', d)               # precio
            if pm:
                c,a=convert_price(pm.group(1), int(pm.group(2)))
                d=f'PR={c}/{a}'
            out.append(base64.b64encode(d.encode()).decode())
        except Exception:
            out.append(s)
    return '<OfferItemID>'+'|'.join(out)+'</OfferItemID>'

xml=open(src,encoding='utf-8').read()
# The capture's own carrier: the first offer's owner.
m=re.search(r'<OwnerCode>([A-Z0-9]{2})</OwnerCode>', xml) or re.search(r'<CarrierDesigCode>([A-Z0-9]{2})</CarrierDesigCode>', xml)
if not m:
    sys.exit('cannot tell the capture\'s carrier: no OwnerCode or CarrierDesigCode')
ref=m.group(1)
xml=re.sub(r'<OfferItemID>([^<]+)</OfferItemID>', recode_offeritemid, xml)
# body: carrier elements and currency-bearing amounts
xml=re.sub(r'(<(?:AirlineDesigCode|CarrierDesigCode|OwnerCode)>)'+ref+r'(</)', rf'\1{code}\2', xml)
# body amounts: <TotalAmount CurCode="XXX">n</...>
def body_amount(m):
    c,a=convert_price(m.group(2), int(m.group(3)))
    return f'{m.group(1)}{c}">{a}</'
xml=re.sub(r'(<(?:TotalAmount|BaseAmount|Amount)[^>]*CurCode=")([A-Z]{3})">(\d+)</', body_amount, xml)
sys.stdout.write(xml)
