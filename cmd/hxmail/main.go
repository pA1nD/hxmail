// Command hxmail prints the mail held in a New Outlook for Mac store (HxStore.hxd) as JSON lines,
// newest first. It snapshots the store before reading and never writes to the profile.
//
//	hxmail [-body] [-profile DIR] [-o FILE] [-attachments DIR] [-watch INTERVAL]
//
// Without -profile it reads the only profile under Outlook's group container and refuses to guess
// when there are several. -o writes the output to FILE through a temporary file renamed into
// place, so a reader never sees a half-written mailbox; -attachments copies every downloaded
// attachment into DIR (mirroring its path under the profile's Files directory) and names the copy
// in the output — for a consumer that may not open the Outlook profile itself. Exit status: 0 complete, 1 error, 3 output written but incomplete (the
// reasons are on stderr).
// -watch keeps one process running and repeats the export whenever the store file changes (checked
// every INTERVAL): macOS asks whether hxmail may access the Outlook profile once per process, so a
// long-lived one is asked once per login rather than on every run.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
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

// errCopies is the errPartial of attachment copies that failed: unlike a skipped record this is
// usually a destination to fix, so a watcher keeps trying. errors.Is(errCopies, errPartial).
var errCopies = fmt.Errorf("%w: attachment copies failed", errPartial)

func main() {
	profile := flag.String("profile", "", "Outlook profile directory (holds HxStore.hxd)")
	withBody := flag.Bool("body", false, "include the body as plain text")
	outPath := flag.String("o", "", "write the output to this file (a temporary file renamed into place) instead of stdout")
	attDir := flag.String("attachments", "", "copy downloaded attachments into this directory and name the copies in the output")
	watch := flag.Duration("watch", 0, "keep running and export again whenever the store changes, checking at this interval (needs -o)")
	flag.Parse()
	// Ctrl-C, a closed terminal or kill cancel the read; the snapshot is removed on the way out.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	var err error
	if *watch > 0 {
		err = watchLoop(ctx, *profile, *withBody, *outPath, *attDir, *watch)
	} else {
		err = run(ctx, *profile, *withBody, *outPath, *attDir)
	}
	switch {
	case errors.Is(err, errPartial):
		os.Exit(exitPartial)
	case err != nil:
		fmt.Fprintln(os.Stderr, "hxmail:", err)
		os.Exit(1)
	}
}

// watchLoop runs the export, then again whenever HxStore.hxd's size or modification time moved,
// looking every interval. The profile is resolved once (symbolic links followed, as run does) and
// must exist. A failed export is reported on stderr and tried again at the next look; an
// incomplete one (exit status 3 otherwise) is reported and tried once more, then left until the
// store changes — a permanent skip must not re-export every interval. A store that cannot be
// examined is reported once per distinct error and polling continues. Ctrl-C, SIGTERM and SIGHUP
// end the loop. Needs -o: on stdout the exports would run into each other.
func watchLoop(ctx context.Context, profile string, withBody bool, outPath, attDir string, interval time.Duration) error {
	if outPath == "" {
		return errors.New("-watch needs -o FILE")
	}
	if profile == "" {
		p, err := defaultProfile()
		if err != nil {
			return err
		}
		profile = p
	}
	resolved, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return fmt.Errorf("profile %s: %w", profile, err)
	}
	profile = resolved
	store := filepath.Join(profile, "HxStore.hxd")
	if _, err := os.Stat(store); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", outlookdesktop.ErrStoreNotFound, store)
		}
		return fmt.Errorf("%s: %w", store, err) // a permission problem is the usual one, and the remedy differs
	}
	var seen os.FileInfo // the store as last exported: size, modification time and identity
	var lastStatErr string
	var lastExport time.Time
	pending := false        // an export is owed at the next look, store change or not
	retriedPartial := false // the one extra try an incomplete export gets per store state
	// a safety net for what the store's own change cannot show: a body file that became
	// readable, a replaced file with identical size and time
	const atLeastEvery = 30 * time.Minute
	for {
		st, err := os.Stat(store)
		if err != nil {
			if msg := err.Error(); msg != lastStatErr {
				fmt.Fprintln(os.Stderr, "hxmail: watch:", err)
				lastStatErr = msg
			}
		} else {
			lastStatErr = ""
			changed := seen == nil || st.Size() != seen.Size() || !st.ModTime().Equal(seen.ModTime()) || !os.SameFile(st, seen)
			if changed {
				seen = st
				retriedPartial = false
			}
			if changed || pending || time.Since(lastExport) > atLeastEvery {
				pending = false
				lastExport = time.Now()
				switch err := run(ctx, profile, withBody, outPath, attDir); {
				case errors.Is(err, errCopies):
					// a destination to fix, not a record to give up on: keep trying
					fmt.Fprintln(os.Stderr, "hxmail: export incomplete (see above)")
					pending = true
				case errors.Is(err, errPartial):
					fmt.Fprintln(os.Stderr, "hxmail: export incomplete (see above)")
					if !retriedPartial {
						retriedPartial = true
						pending = true
					}
				case ctx.Err() != nil:
					return nil
				case err != nil:
					fmt.Fprintln(os.Stderr, "hxmail:", err)
					pending = true
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
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

func run(ctx context.Context, profile string, withBody bool, outPath, attDir string) error {
	// Copies left by runs that were killed outright (kill -9, a crash) are removed here.
	outlookdesktop.SweepStale(time.Hour)
	if profile == "" {
		p, err := defaultProfile()
		if err != nil {
			return err
		}
		profile = p
	}
	// The profile directory is resolved once, the way the kernel resolves it (a symbolic link
	// followed by ".." climbs from the link's target), and that one directory serves the reads
	// and the guard below alike — filepath.Join would clean "link/.." lexically otherwise.
	if p, err := filepath.EvalSymlinks(profile); err == nil {
		profile = p
	}
	// The export flags are the only writes hxmail ever makes; none of them may land inside the
	// Outlook profile (-o <profile>/HxStore.hxd would replace the live store with JSON). The
	// paths are canonicalized here and those exact paths are what gets written.
	outPath, attDir, err := exportGuard(profile, outPath, attDir)
	if err != nil {
		return err
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
				if err := ctx.Err(); err != nil {
					return err
				}
				if dst, err := copyAttachment(ctx, profile, attDir, a.Path); err == nil {
					att.Path = dst
				} else if ctx.Err() != nil {
					return ctx.Err()
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
			if err := ctx.Err(); err != nil {
				return err
			}
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
		if copyFailed > 0 {
			return errCopies
		}
		return err
	}
	if copyFailed > 0 {
		return errCopies
	}
	return nil
}

// writeAtomic writes what emit produces to path through a temporary file beside it (a short
// random name, so a path near the filesystem's name limit still works), synced and renamed into
// place: a reader of path sees the previous complete file or the new one, never a partial write.
// The temporary file is removed on any failure.
func writeAtomic(path string, emit func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".hxmail-*") // 0600, O_EXCL
	if err != nil {
		return err
	}
	tmp := f.Name()
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

// canonPath resolves p the way the kernel resolves it when the entry is created: every existing
// directory on the way is resolved, symbolic links included — so "link/.." climbs from the link's
// target, not from the link, unlike filepath.Clean — and the entry itself is never followed. It
// is the one form a destination takes, for the containment check and for the write alike.
func canonPath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd + string(filepath.Separator) + p
	}
	parts := strings.Split(p, string(filepath.Separator))
	dir, entry := parts[:len(parts)-1], parts[len(parts)-1]
	for n := len(dir); n >= 1; n-- {
		prefix := strings.Join(dir[:n], string(filepath.Separator))
		if prefix == "" {
			prefix = string(filepath.Separator)
		}
		if _, err := os.Stat(prefix); err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(prefix)
		if err != nil {
			return "", err
		}
		rest := append([]string{real}, dir[n:]...)
		return filepath.Join(append(rest, entry)...), nil
	}
	return "", fmt.Errorf("%s: no existing directory on the way", p)
}

// exportGuard canonicalizes the export destinations and refuses any that would land inside the
// Outlook profile. -o names a file (its directory entry is what gets written); -attachments
// names a directory that files go below, so it is checked as the parent of a file.
func exportGuard(profile, outPath, attDir string) (string, string, error) {
	check := func(flag, p string) (string, error) {
		c, err := canonPath(p)
		if err != nil {
			return "", fmt.Errorf("%s: %w", flag, err)
		}
		inside, err := insideDir(profile, c)
		if err != nil {
			return "", fmt.Errorf("%s: %w", flag, err)
		}
		if inside {
			return "", fmt.Errorf("%s is inside the Outlook profile; hxmail never writes there", flag)
		}
		return c, nil
	}
	if outPath != "" {
		c, err := check("-o", outPath)
		if err != nil {
			return "", "", err
		}
		outPath = c
	}
	if attDir != "" {
		// not filepath.Join: it would clean a trailing ".." away before canonPath sees it
		c, err := check("-attachments", attDir+string(filepath.Separator)+"x")
		if err != nil {
			return "", "", err
		}
		attDir = filepath.Dir(c)
	}
	return outPath, attDir, nil
}

// insideDir reports whether the directory entry p — which need not exist yet — would be
// created inside dir: in dir itself or anywhere below it. It compares directory identities, not
// path text, so a relative path, a symbolic link on the way or a differently-cased alias on a
// case-insensitive volume all count. The entry p itself is never followed: a rename replaces the
// entry, not what a link there points to. An error means dir or the existing part of p could not
// be examined.
func insideDir(dir, p string) (bool, error) {
	base, err := os.Stat(dir)
	if err != nil {
		return false, err
	}
	if !base.IsDir() {
		return false, fmt.Errorf("%s is not a directory", dir)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return false, err
	}
	// the longest existing prefix of the entry's parent
	exist := filepath.Dir(abs)
	for {
		if _, err := os.Stat(exist); err == nil {
			break
		}
		parent := filepath.Dir(exist)
		if parent == exist {
			return false, nil
		}
		exist = parent
	}
	real, err := filepath.EvalSymlinks(exist)
	if err != nil {
		return false, err
	}
	for cur := real; ; cur = filepath.Dir(cur) {
		if fi, err := os.Stat(cur); err == nil && os.SameFile(fi, base) {
			return true, nil
		}
		if filepath.Dir(cur) == cur {
			return false, nil
		}
	}
}

// ctxReader stops a copy as soon as ctx is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// sameContent reports whether the file at path holds exactly the bytes r yields.
func sameContent(r io.Reader, path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // the caller's own export directory
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	a, b := make([]byte, 1<<16), make([]byte, 1<<16)
	for {
		na, ea := io.ReadFull(r, a)
		nb, eb := io.ReadFull(f, b)
		if ea != nil && !errors.Is(ea, io.EOF) && !errors.Is(ea, io.ErrUnexpectedEOF) {
			return false, ea
		}
		if eb != nil && !errors.Is(eb, io.EOF) && !errors.Is(eb, io.ErrUnexpectedEOF) {
			return false, eb
		}
		if na != nb || !bytes.Equal(a[:na], b[:nb]) {
			return false, nil
		}
		if na < len(a) { // a short read is the end of both
			return true, nil
		}
	}
}

// copyAttachment copies the attachment file that storePath (as ~/Files/...) names under the
// profile into dir, at the same relative path, and returns the copy's absolute path. The source
// is resolved with the containment checks of outlookmail.ResolveFile and opened read-only. A
// copy that already exists with the same bytes is kept; anything else is replaced through a
// temporary file renamed into place. Cancelling ctx stops the copy.
func copyAttachment(ctx context.Context, profile, dir, storePath string) (string, error) {
	src, err := outlookmail.ResolveFile(profile, storePath)
	if err != nil {
		return "", err
	}
	rel := strings.TrimPrefix(storePath, "~/Files/")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dst, err := canonPath(filepath.Join(dir, filepath.FromSlash(rel)))
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
	// the destination must land below dir and outside the profile even through a symbolic link
	// that an earlier run or someone else left inside the export directory
	if ok, err := insideDir(dir, dst); err != nil || !ok {
		return "", errors.New("hxmail: attachment destination leaves the export directory")
	}
	if in, err := insideDir(profile, dst); err != nil || in {
		return "", errors.New("hxmail: attachment destination is inside the Outlook profile")
	}
	// only a regular file is reused (a link at dst — to the original, say — is replaced: the
	// copy must stand on its own, readable without the profile)
	if have, err := os.Lstat(dst); err == nil && have.Mode().IsRegular() && have.Size() == st.Size() {
		same, err := sameContent(ctxReader{ctx, in}, dst)
		if err == nil && same {
			return dst, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if _, err := in.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
	}
	err = writeAtomic(dst, func(w io.Writer) error {
		_, err := io.Copy(w, ctxReader{ctx, in})
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
