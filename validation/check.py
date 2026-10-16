"""Compare hxmail output against expected.json (sent mail) and states.template.json (Outlook actions).

Usage: ../bin/hxmail -body | python3 -I check.py
Prints one PASS/FAIL line per checked field and exits non-zero on any failure.
"""
import json, sys, pathlib

HERE = pathlib.Path(__file__).parent
msgs = [json.loads(l) for l in sys.stdin if l.startswith("{")]
by_subject = {}
for m in msgs:
    by_subject.setdefault(m["subject"].strip(), []).append(m)

expected = json.loads((HERE / "expected.json").read_text())
# The Outlook actions of the README, with {placeholders} filled from config.json.
cfg = json.loads((HERE / "config.json").read_text())
states = json.loads((HERE / "states.template.json").read_text())
fill = lambda v: [a.format(**cfg) for a in v] if isinstance(v, list) else v
for subj, st in states.items():
    expected.setdefault(subj, {}).update({k: fill(v) for k, v in st.items()})

addrs = lambda people: sorted(p["address"].lower() for p in people or [])
fails = 0


def check(subj, field, got, want):
    global fails
    ok = got == want
    fails += not ok
    print(f"{'PASS' if ok else 'FAIL'}  {subj[:40]:40}  {field:15} got={got!r}" + ("" if ok else f" want={want!r}"))


for subj, exp in expected.items():
    found = by_subject.get(subj, [])
    if exp.get("folder_kind") is not None:  # a moved message: pick the copy in that folder
        found = [m for m in found if m["folder_kind"] == exp["folder_kind"]] or found
    if len(found) != 1:
        fails += 1
        print(f"FAIL  {subj[:40]:40}  {'present':15} found {len(found)} copies")
        continue
    m = found[0]
    for role in ("to", "cc", "bcc", "reply_to"):
        if role in exp:
            check(subj, role, addrs(m.get(role)), sorted(a.lower() for a in exp[role]))
    for k in ("importance", "has_attachments", "read", "flag", "folder_kind"):
        if k in exp:
            check(subj, k, m[k], exp[k])
    if "attachments" in exp:
        check(subj, "attachments", sorted(a["name"] for a in m.get("attachments") or []), sorted(exp["attachments"]))
    if "body_contains" in exp:
        check(subj, "body", exp["body_contains"] in (m.get("body") or ""), True)
    if "other_recipients" not in exp:
        check(subj, "unroled", m.get("other_recipients"), None)

print(f"\n{len(msgs)} messages read, {fails} failures")
sys.exit(1 if fails else 0)
