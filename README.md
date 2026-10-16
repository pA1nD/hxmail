# hxmail

Reads the mail that New Outlook for Mac keeps offline (`HxStore.hxd`) and prints it as JSON lines,
newest first: folder, sender, To/Cc/Bcc/Reply-To, subject, dates, read state, flag, importance,
attachments and, with `-body`, the body as plain text.

```sh
./hxmail                  # the messages of the account in the local store
./hxmail -body            # plus the body as plain text
./hxmail -profile DIR     # a profile directory, required when there are several
./hxmail -o FILE          # write to FILE through a temporary file renamed into place
./hxmail -attachments DIR # copy downloaded attachments into DIR and name the copies in the output
./hxmail -watch 2m -o FILE # keep running; export again whenever the store changes (checked every 2 min)
```

`-o` and `-attachments` are for handing the mailbox to another program that may not open the
Outlook profile itself (macOS guards it as another app's data: a daemon that was not allowed gets
EPERM). Run `hxmail` from launchd, allow it when macOS asks whether it may access data from other
apps, and the consumer reads the file and the copied attachments from a folder of your choosing. An attachment whose copy fails keeps its
`~/Files/…` path in the output and counts as a skip (exit status 3).

`hxmail` copies the store into a private temporary directory, reads the copy and removes it before
printing anything; a copy left by a run that was killed outright is removed by the next run. It
never writes to the Outlook profile, makes no network connection and starts no other program.

Exit status: `0` complete, `1` error, `3` output written but incomplete. What was skipped is
printed to stderr: records in a layout it does not know, messages without their detail record, or
messages of a second account in the same store (only the account with the most mail is read).
Blocks that fail their checksum are reported as a warning: a live store keeps stale regions, so a
few are normal.

## Why

New Outlook for Mac keeps your mail in a local file but offers no export and no scripting access
to it. `hxmail` is an interoperability tool: it reads the data files on your own machine so that
your own mail can be used in other software.

It contains no Microsoft code. The reader derives from [m365crawl](https://github.com/ourostack/m365crawl),
which builds on published format research for the store's block container. The additions in this
project (see below) were found by comparing the store file with the Exchange sync logs Outlook
writes into the same profile; no Microsoft software was decompiled or disassembled for them.

## Install

With Go 1.27 or later:

```sh
go install github.com/pA1nD/hxmail/cmd/hxmail@latest   # into $(go env GOPATH)/bin
```

or from a checkout, `go build -o hxmail ./cmd/hxmail` and run `./hxmail`.

Without a local Go toolchain, build in Docker. Set `ARCH=arm64` on Apple silicon, `ARCH=amd64` on
an Intel Mac:

```sh
ARCH=arm64
COPYFILE_DISABLE=1 tar -c --exclude upstream --exclude bin . | docker run --rm -i -e ARCH=$ARCH golang:1.27.2 sh -c \
  'mkdir /src && cd /src && tar -x && go test ./... >&2 && GOOS=darwin GOARCH=$ARCH CGO_ENABLED=0 go build -trimpath -o /tmp/hxmail ./cmd/hxmail >&2 && cat /tmp/hxmail' > hxmail
chmod +x hxmail && codesign -s - -f hxmail
```

macOS asks whether `hxmail` may access data from other apps (the Outlook profile). It asks once per
process, whatever the binary's signature or packaging (measured on macOS 15 with an ad-hoc
signature, a trusted self-signed identity and an app bundle alike), so a job that starts hxmail
afresh every few minutes is asked every few minutes. Run it as one long-lived process instead —
`hxmail -watch 2m -o FILE -attachments DIR` under launchd with `KeepAlive` — and the question comes
once per login. Full Disk Access for the binary removes the question entirely; so does running from
a terminal that already has it.

(`COPYFILE_DISABLE=1` stops macOS tar from adding `._*` resource-fork entries, which the tests
would otherwise read as corrupt fixtures.)

To run it as `hxmail`, move the binary into a directory on your `PATH`, for example
`mv hxmail /usr/local/bin/`.

## Format coverage

Measured on Outlook for Mac 16.114, against the Exchange sync log for every field below.

| Field | Status |
|---|---|
| Sender, subject, preview, received and sent time, Message-ID | read |
| To, Cc, Reply-To | read; Bcc only in the sender's copy (Sent Items), as Exchange stores it |
| Folder and folder kind (inbox, sent, drafts, deleted, archive, junk) | read; junk only in the 16.114 folder layout |
| Read state, flag (none, flagged, complete), importance | read |
| Attachments (name, size, local path) and the has-attachments flag; inline images excluded | read |
| Body: inline HTML or the separate body file for large messages | read with `-body` |

The store is a cache. It holds what Outlook has synced, and many messages carry only a preview
until they are opened. A store whose mail records are in a layout `hxmail` does not know is
refused; anything it skips in a store it does read is reported (exit status `3`).

## Known limitations

- One account per store: a second account in the same profile is skipped and reported.
- Memory: every body is held until the output is written, up to 16 MiB each. `-body` on a very
  large mailbox needs memory in proportion.
- Body files are opened after a containment check on their resolved path. Someone who can already
  write to your Outlook profile could swap a path component in between; reading through `os.Root`
  would close that gap.
- Field positions were measured on Outlook 16.114 and 16.115. An update can move them; the
  validation suite below shows what to re-measure.

## Changes from m365crawl

`internal/` is derived from m365crawl (MIT, see `THIRD_PARTY_NOTICES.md`). `upstream/` holds the
unmodified files: `git diff --no-index upstream/internal internal` shows every change.

| Change | Where | Evidence |
|---|---|---|
| Folder layout of 16.114 (tag 0x4d8, fields 8 bytes later) accepted beside 16.115's 0x4d0 | `folder.go`, `collect.go` | all named folders |
| Junk folder: well-known folder id 7 at folder +1192 (16.114 layout) | `folder.go` | server folder ids |
| Recipient role at +28: 0xcc to, 0xcd cc, 0xce bcc, 0x19d reply-to | `message.go` | server recipient lists |
| Attachment byte +740 == 2 marks an inline part. A Content-ID alone does not (Gmail sets one on ordinary attachments), nor does a GUID-like name (upstream's rule) | `attachment.go` | server Inline flags |
| Header byte +1032 is the has-attachments flag | `message.go` | server HasAttachment |
| Only a message's newest recipient set counts: each draft save leaves an older full set behind | `collect.go` | a draft saved three times |
| Copies of a record are ranked in one total order: a clean copy or a resynced one that repeats its key at +40 beats one that does not; for a body record a copy with content beats an empty one; then the newer change stamp; then the later block; clean over resynced only at the same place. Upstream kept any clean copy over a newer resynced one, which showed stale flags, and a message's first, empty body record over the body written when the message was opened | `collect.go`, tests for every read order | every mail object repeats its key; 10 of 36 opened messages without a body on a live store |
| Walk-coverage floor 70% (upstream 80%) | `collect.go`, test adjusted | a live store at 79.7% with every mail check passing |

Also: snapshots are named `hxmail-snapshot-*` and swept by `SweepStale` (upstream relied on its
Teams code for the sweep); the command removes the snapshot before printing and on Ctrl-C.

Dropped from the derived tree: `hxfixture`, `cmd/hxstat` and one Teams snapshot test, which need
m365crawl's repository-level fixtures or Teams code.

## Validation

`validation/` checks `hxmail` end to end against real mail.

1. Copy `validation/config.example.json` to `validation/config.json` and fill in the Outlook address
   under test, a sender address and two more addresses you can read. Sending uses the Gmail API
   through the command in `send_command` (by default Google's `gws` CLI, which must be installed
   and signed in to the sender's account).
2. Run `python3 -I validation/send_tests.py` to send T0 (Bcc and a real attachment), T1 (several
   recipients, Reply-To, high importance, inline image and a real attachment) and T2 (large body,
   low importance). `--eml` writes them to `validation/out/` instead.
3. In Outlook: open T1 and flag it; flag T0 and mark it complete; move T2 unread to Junk Email; send
   `hxmail T3 sent roles` To the sender address, Cc the second, Bcc the third.
4. Run `hxmail -body | python3 -I validation/check.py`. It prints PASS or FAIL per field.

## Trademarks

Microsoft, Outlook and Exchange are trademarks of Microsoft Corporation. This project is not
affiliated with or endorsed by Microsoft.

## Licence

MIT, see `LICENSE`. Third-party code: `THIRD_PARTY_NOTICES.md`.
