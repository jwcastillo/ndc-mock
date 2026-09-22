#!/usr/bin/env python3
"""Shared TypeSafe access for the mapping tools.

derive-mapping.py judges names, compare-paths.py judges paths; both ask one
Choice per unresolved item and route the answer by its own confidence. What
they share is the transport and that routing rule, and nothing else: each tool
writes its own questions, because the evidence it holds is what makes them
worth asking.

stdlib only, on purpose - these tools run on a machine that has the IATA
schemas on it, not in a virtualenv someone has to build first.
"""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request

URL = "https://api.typesafe.ai/v1/systemone"
QUESTIONS_PER_CALL = 32
DEFAULT_MODEL = "jev-latest"
DEFAULT_THRESHOLD = 0.8


def api_key() -> str:
    key = os.environ.get("TYPESAFE_API_KEY")
    if not key:
        sys.exit("TYPESAFE_API_KEY is not set. Export it from a gitignored *.env, the way\n"
                 "the provider credentials are handled; get a key at https://console.typesafe.ai/")
    return key


def post(payload: dict, key: str) -> dict:
    """The key lives in this header and nowhere else: not in argv, not in the output."""
    req = urllib.request.Request(
        URL,
        data=json.dumps(payload).encode(),
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        # Report the status, never the request: a traceback here would carry the header.
        sys.exit(f"TypeSafe returned {e.code} {e.reason}")
    except urllib.error.URLError as e:
        sys.exit(f"TypeSafe unreachable: {e.reason}")


def ask(state: dict, questions: dict[str, dict], model: str = DEFAULT_MODEL) -> dict[str, dict]:
    """Ask every question about one state, in as few requests as the limit allows.

    Questions over the same state are independent and run in parallel, so one
    request of many questions costs far less than many requests of one.
    """
    key, ids, answers = api_key(), list(questions), {}
    for i in range(0, len(ids), QUESTIONS_PER_CALL):
        chunk = {qid: questions[qid] for qid in ids[i:i + QUESTIONS_PER_CALL]}
        answers |= post({"state": state, "model": model, "questions": chunk}, key)["answers"]
    return answers


def verdict(answer: dict, threshold: float = DEFAULT_THRESHOLD) -> tuple[str, float, float]:
    """(choice, confidence, probability), with the choice replaced by "" below threshold.

    Confidence decides whether to act on the answer; the answer decides what.
    An empty choice is the signal to hand the item to a person.
    """
    choice, confidence = answer["choice"], answer["confidence"]
    probability = answer["probabilities"][choice]
    return ("" if confidence < threshold else choice), round(confidence, 3), round(probability, 3)
