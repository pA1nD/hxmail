// Command hxmail prints the mail held in a New Outlook for Mac store (HxStore.hxd) as JSON lines,
// newest first. It snapshots the store before reading and never writes to the profile.
//
//	hxmail [-body] [-profile DIR]
//
// Without -profile it reads the only profile under Outlook's group container and refuses to guess
// when there are several. Exit status: 0 complete, 1 error, 3 output written but incomplete (the
// reasons are on stderr).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	flag.Parse()
	err := run(*profile, *withBody)
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

func run(profile string, withBody bool) error {
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
			msg.Attachments = append(msg.Attachments, attachment{a.Name, a.Size, a.Downloaded, a.Path})
		}
		if withBody && len(m.Body.HTML) > 0 {
			msg.Body = outlookmail.HTMLText(m.Body.HTML)
		}
		out = append(out, msg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Received.After(out[j].Received) })

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	for _, m := range out {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "hxmail: %d messages, %d folders; %d blocks verified of %d\n",
		len(out), len(res.Folders), res.Stats.BlocksValid, res.Stats.BlocksFound)
	return report(res)
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
