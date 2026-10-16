"""Build the hxmail validation emails and send them to the Outlook account under test.

Each message exercises fields the reader maps; expected.json records what hxmail must report.
Addresses come from config.json (copy config.example.json). The mails are sent through the Gmail
API with the command in config.json's "send_command" (for example the gws CLI); with --eml they
are written to out/*.eml instead, to send by any other means.

Usage: python3 -I send_tests.py [--dry-run | --eml]  (--help lists the options)
"""
import argparse, base64, json, subprocess, pathlib, email.utils
from email.message import EmailMessage

HERE = pathlib.Path(__file__).parent
CFG = json.loads((HERE / "config.json").read_text())
TARGET, SENDER = CFG["outlook_address"], CFG["sender_address"]
SECOND, THIRD = CFG["second_address"], CFG["third_address"]
PNG = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
PDF = b"%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj 2 0 obj<</Type/Pages/Kids[]/Count 0>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n"


def base(subject):
    m = EmailMessage()
    m["From"] = SENDER
    m["Subject"] = subject
    m["Date"] = email.utils.formatdate(localtime=False)
    return m


def t0():
    """The account under test only in Bcc, with one real attachment."""
    m = base("hxmail T0 bcc received")
    m["To"] = SENDER
    m["Bcc"] = TARGET
    m.set_content("T0: the account under test is in Bcc. HXMAIL-T0-END")
    m.add_attachment(b"hxmail attachment test\n", maintype="text", subtype="plain", filename="t0-test.txt")
    exp = {"to": [SENDER], "cc": [], "bcc": [], "has_attachments": True, "attachments": ["t0-test.txt"]}
    return m, exp


def t1():
    """Two To, a Cc, a Reply-To, high importance, non-ASCII subject, inline image plus a real file."""
    m = base("hxmail T1 – Größe ✓ rich")
    m["To"] = f"{TARGET}, Second Test <{SECOND}>"
    m["Cc"] = f"Third Test <{THIRD}>"
    m["Reply-To"] = f"Reply Test <{SECOND}>"
    m["Importance"] = "high"
    m["X-Priority"] = "1"
    m.set_content("T1 plain part")
    m.add_alternative('<html><body><p>T1 html with inline image <img src="cid:t1logo@hxmail"> HXMAIL-T1-END</p></body></html>', subtype="html")
    m.get_payload()[1].add_related(PNG, "image", "png", cid="<t1logo@hxmail>", filename="t1logo.png", disposition="inline")
    m.add_attachment(PDF, maintype="application", subtype="pdf", filename="t1-report.pdf")
    exp = {"to": [TARGET, SECOND], "cc": [THIRD], "reply_to": [SECOND], "bcc": [], "importance": "high",
           "has_attachments": True, "attachments": ["t1-report.pdf"], "body_contains": "HXMAIL-T1-END"}
    return m, exp


def t2():
    """A body large enough to be stored as a separate file, low importance, no attachments."""
    m = base("hxmail T2 large body low importance")
    m["To"] = TARGET
    m["Importance"] = "low"
    m["X-Priority"] = "5"
    para = "<p>" + "Large body filler line for the HxStore body-file path. " * 20 + "</p>\n"
    m.set_content("T2 plain part")
    m.add_alternative("<html><body>" + para * 300 + "<p>HXMAIL-T2-END</p></body></html>", subtype="html")
    exp = {"to": [TARGET], "cc": [], "reply_to": [], "bcc": [], "importance": "low",
           "has_attachments": False, "attachments": [], "body_contains": "HXMAIL-T2-END"}
    return m, exp


def main():
    ap = argparse.ArgumentParser(description="Build the hxmail validation emails and send them (see README).")
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--dry-run", action="store_true", help="build and list the mails, send nothing")
    mode.add_argument("--eml", action="store_true", help="write the mails to out/*.eml instead of sending")
    args = ap.parse_args()  # --help and unknown arguments exit here, before anything is sent
    expected = {}
    for build in (t0, t1, t2):
        msg, exp = build()
        expected[msg["Subject"]] = exp
        raw = msg.as_bytes()
        print(msg["Subject"], f"{len(raw)} bytes")
        if args.eml:
            out = HERE / "out"
            out.mkdir(exist_ok=True)
            (out / f"{build.__name__}.eml").write_bytes(raw)
        elif not args.dry_run:
            body = json.dumps({"raw": base64.urlsafe_b64encode(raw).decode()})
            res = subprocess.run(CFG["send_command"] + ["--params", '{"userId":"me"}', "--json", body],
                                 capture_output=True, text=True, check=True).stdout
            print("  sent", json.loads(res[res.index("{"):])["id"])
    (HERE / "expected.json").write_text(json.dumps(expected, indent=1, ensure_ascii=False))


if __name__ == "__main__":
    main()
