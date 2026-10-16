// Command hxmail prints the mail held in a New Outlook for Mac store (HxStore.hxd) as JSON lines,
// newest first. It snapshots the store before reading and never writes to the profile.
//
//	hxmail [-body] [-profile DIR] [-o FILE] [-attachments DIR]
//
// Without -profile it reads the only profile under Outlook's group container and refuses to guess
// when there are several. -o writes the output to FILE through a temporary file renamed into
// place, so a reader never sees a half-written mailbox; -attachments copies every downloaded
// attachment into DIR (mirroring its path under the profile's Files directory) and names the copy
// in the output — for a consumer that may not open the Outlook profile itself. Exit status: 0 complete, 1 error, 3 output written but incomplete (the
// reasons are on stderr).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pA1nD/hxmail/internal/hxstore"
	"github.com/pA1nD/hxmail/internal/outlookdesktop"
	"github.com/pA1nD/hxmail/internal/outlookmail"
)

type person struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

type attachment struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Downloaded bool   `json:"downloaded"`
	Path       string `json:"path,omitempty"`
}

type message struct {
	Received       time.Time    `json:"received"`
	Sent           time.Time    `json:"sent,omitzero"`
	Folder         string       `json:"folder"`
	FolderKind     string       `json:"folder_kind"`
	From           person       `json:"from"`
	To             []person     `json:"to,omitempty"`
	Cc             []person     `json:"cc,omitempty"`
	Bcc            []person     `json:"bcc,omitempty"`
	ReplyTo        []person     `json:"reply_to,omitempty"`
	Other          []person     `json:"other_recipients,omitempty"`
	Subject        string       `json:"subject"`
	Read           string       `json:"read"`
	Flag           string       `json:"flag"`
	Importance     string       `json:"importance"`
	HasAttachments bool         `json:"has_attachments"`
	Attachments    []attachment `json:"attachments,omitempty"`
	MessageID      string       `json:"message_id,omitempty"`
	InReplyTo      string       `json:"in_reply_to,omitempty"`
	Preview        string       `json:"preview,omitempty"`
	BodyState      string       `json:"body_state"`
	Body           string       `json:"body,omitempty"`
}

// exitPartial is the exit status of a read that wrote its output but skipped data.
const exitPartial = 3

// errPartial says the output was written but data was skipped; the reasons are already on stderr.
var errPartial = errors.New("incomplete read")

func main() {
	profile := flag.String("profile", "", "Outlook profile directory (holds HxStore.hxd)")
	withBody := flag.Bool("body", false, "include the body as plain text")
	outPath := flag.String("o", "", "write the output to this file (a temporary file renamed into place) instead of stdout")
	attDir := flag.String("attachments", "", "copy downloaded attachments into this directory and name the copies in the output")
	flag.Parse()
	err := run(*profile, *withBody, *outPath, *attDir)
	switch {
	case errors.Is(err, errPartial):
		os.Exit(exitPartial)
	case err != nil:
		fmt.Fprintln(os.Stderr, "hxmail:", err)
		os.Exit(1)
	}
}

// defaultProfile is the only Outlook profile that holds a store. Several are an error: the
// caller names one with -profile.
func defaultProfile() (string, error) {
	root, err := outlookdesktop.DefaultRoot()
	if err != nil {
		return "", err
	}
	profiles, _, _, err := outlookdesktop.Discover(root)
	if err != nil {
		return "", err
	}
	switch len(profiles) {
	case 0:
		return "", fmt.Errorf("no Outlook profile with an HxStore.hxd under %s", root)
	case 1:
		return profiles[0].Dir, nil
	}
	var dirs []string
	for _, p := range profiles {
		dirs = append(dirs, p.Dir)
	}
	return "", fmt.Errorf("several Outlook profiles, choose one with -profile:\n  %s", strings.Join(dirs, "\n  "))
}

func run(profile string, withBody bool, outPath, attDir string) error {
	// Ctrl-C, a closed terminal or kill cancel the read; the snapshot is removed on the way out.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// Copies left by runs that were killed outright (kill -9, a crash) are removed here.
	outlookdesktop.SweepStale(time.Hour)
	if profile == "" {
		p, err := defaultProfile()
		if err != nil {
			return err
		}
		profile = p
	}
	info, cleanup, err := outlookdesktop.Snapshot(ctx, filepath.Join(profile, "HxStore.hxd"))
	defer cleanup()
	if err != nil {
		return err
	}
	s, err := hxstore.OpenFile(info.Path)
	if err != nil {
		return err
	}
	defer s.Close()
	res, err := outlookmail.Collect(ctx, s, profile, "", outlookmail.Options{ReadBodies: withBody})
	// Everything is read: the copy of the mailbox goes before any output, so a reader that stops
	// early (| head) cannot leave it behind.
	_ = s.Close()
	cleanup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hxmail: walk stats %+v\n", res.Stats)
		return err
	}

	out := make([]message, 0, len(res.Messages))
	copyFailed := 0
	for _, m := range res.Messages {
		msg := message{
			Received: m.Received, Sent: m.Sent,
			Folder: m.Folder.Name, FolderKind: m.Folder.Kind,
			From:    person{m.SenderName, m.SenderAddress},
			Subject: m.Subject, Read: m.ReadState, Flag: m.Flag, Importance: m.Importance,
			MessageID: m.MessageID, InReplyTo: m.InReplyTo, Preview: m.Preview,
			BodyState: m.Body.State,
		}
		for _, r := range m.Recipients {
			p := person{r.Name, r.Address}
			switch r.Role {
			case "to":
				msg.To = append(msg.To, p)
			case "cc":
				msg.Cc = append(msg.Cc, p)
			case "bcc":
				msg.Bcc = append(msg.Bcc, p)
			case "reply_to":
				msg.ReplyTo = append(msg.ReplyTo, p)
			default:
				msg.Other = append(msg.Other, p)
			}
		}
		if m.HasAttachments != nil {
			msg.HasAttachments = *m.HasAttachments
		}
		for _, a := range m.Attachments {
			if a.Inline {
				continue
			}
			msg.HasAttachments = true
			att := attachment{a.Name, a.Size, a.Downloaded, a.Path}
			if attDir != "" && a.Downloaded && a.Path != "" {
				if dst, err := copyAttachment(profile, attDir, a.Path); err == nil {
					att.Path = dst
				} else {
					copyFailed++
				}
			}
			msg.Attachments = append(msg.Attachments, att)
		}
		if withBody && len(m.Body.HTML) > 0 {
			msg.Body = outlookmail.HTMLText(m.Body.HTML)
		}
		out = append(out, msg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Received.After(out[j].Received) })

	emit := func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		for _, m := range out {
			if err := enc.Encode(m); err != nil {
				return err
			}
		}
		return nil
	}
	if outPath != "" {
		if err := writeAtomic(outPath, emit); err != nil {
			return err
		}
	} else if err := emit(os.Stdout); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "hxmail: %d messages, %d folders; %d blocks verified of %d\n",
		len(out), len(res.Folders), res.Stats.BlocksValid, res.Stats.BlocksFound)
	if copyFailed > 0 {
		fmt.Fprintf(os.Stderr, "hxmail: skipped: %d attachment copies failed (the ~/Files path is kept in the output)\n", copyFailed)
	}
	if err := report(res); err != nil {
		return err
	}
	if copyFailed > 0 {
		return errPartial
	}
	return nil
}

// writeAtomic writes what emit produces to path through a temporary file beside it, synced and
// renamed into place, so a reader of path sees the previous complete file or the new one, never
// a partial write. The temporary file is removed on any failure.
func writeAtomic(path string, emit func(io.Writer) error) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // beside the caller's own output file
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = emit(f); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// copyAttachment copies the attachment file that storePath (as ~/Files/...) names under the
// profile into dir, at the same relative path, and returns the copy's absolute path. The source
// is resolved with the containment checks of outlookmail.ResolveFile and opened read-only; a copy
// that already exists with the source's size is kept as is. The copy is written through a
// temporary file renamed into place.
func copyAttachment(profile, dir, storePath string) (string, error) {
	src, err := outlookmail.ResolveFile(profile, storePath)
	if err != nil {
		return "", err
	}
	rel := strings.TrimPrefix(storePath, "~/Files/")
	dst, err := filepath.Abs(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	in, err := outlookdesktop.OpenReadOnly(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("hxmail: attachment is not a regular file")
	}
	if have, err := os.Stat(dst); err == nil && have.Mode().IsRegular() && have.Size() == st.Size() {
		return dst, nil
	}
	err = writeAtomic(dst, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
	if err != nil {
		return "", err
	}
	return dst, nil
}

// report prints what the read skipped and returns errPartial when messages or their parts may be
// missing. Damaged blocks alone are a warning: a live store keeps stale regions that fail their
// checksum, so a few are normal, but they can also have held mail.
func report(res outlookmail.Result) error {
	partial := false
	for _, l := range res.Losses {
		if l.Code == outlookmail.CodeBlocksDamaged {
			fmt.Fprintf(os.Stderr, "hxmail: warning: %d blocks failed their checksum and were skipped\n", l.Count)
			continue
		}
		fmt.Fprintf(os.Stderr, "hxmail: skipped: %s x%d\n", l.Code, l.Count)
		partial = true
	}
	n := res.Notes
	for _, c := range []struct {
		n    int
		what string
	}{
		{n.MissingDetail, "messages whose detail record is missing"},
		{n.OtherRoot, "messages of another account in this store (only the account with the most mail is read)"},
	} {
		if c.n > 0 {
			fmt.Fprintf(os.Stderr, "hxmail: skipped: %d %s\n", c.n, c.what)
			partial = true
		}
	}
	if partial {
		return errPartial
	}
	return nil
}
