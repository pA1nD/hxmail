package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAtomicLeavesOnlyTheFinalFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sub", "mail.jsonl")
	if err := writeAtomic(out, func(w io.Writer) error { _, err := w.Write([]byte("one\n")); return err }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "one\n" {
		t.Fatalf("content %q", got)
	}
	// a failing emit removes its temporary file and keeps the previous output
	err := writeAtomic(out, func(w io.Writer) error { _, _ = w.Write([]byte("half")); return io.ErrUnexpectedEOF })
	if err == nil {
		t.Fatal("expected the emit error")
	}
	if got, _ := os.ReadFile(out); string(got) != "one\n" {
		t.Fatalf("previous output lost: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(out))
	if len(entries) != 1 {
		t.Fatalf("temporary file left behind: %v", entries)
	}
}

func TestCopyAttachmentMirrorsThePathAndRefusesEscapes(t *testing.T) {
	profile := t.TempDir()
	src := filepath.Join(profile, "Files", "S0", "3", "Attachments", "0", "report[65].pdf")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 300), 0o400); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx := context.Background()
	dst, err := copyAttachment(ctx, profile, dir, "~/Files/S0/3/Attachments/0/report[65].pdf")
	if err != nil {
		t.Fatal(err)
	}
	realDir, _ := filepath.EvalSymlinks(dir) // macOS: /var/folders is /private/var/folders
	want := filepath.Join(realDir, "S0", "3", "Attachments", "0", "report[65].pdf")
	if dst != want {
		t.Fatalf("copy at %s, want %s", dst, want)
	}
	if got, _ := os.ReadFile(dst); len(got) != 300 {
		t.Fatalf("copied %d bytes", len(got))
	}
	// a second run keeps the existing copy
	if again, err := copyAttachment(ctx, profile, dir, "~/Files/S0/3/Attachments/0/report[65].pdf"); err != nil || again != dst {
		t.Fatalf("second copy: %s %v", again, err)
	}
	for _, bad := range []string{"~/Files/../HxStore.hxd", "/etc/passwd", "Files/S0/3/x", "~/Files/S0/../../../x"} {
		if _, err := copyAttachment(ctx, profile, dir, bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(entries) != 1 || !strings.HasSuffix(entries[0], "S0") {
		t.Fatalf("unexpected files in the copy dir: %v", entries)
	}
}

func TestCopyAttachmentReplacesACopyWithOtherBytesOfTheSameSize(t *testing.T) {
	profile, dir := t.TempDir(), t.TempDir()
	src := filepath.Join(profile, "Files", "a.bin")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, bytes.Repeat([]byte("A"), 70000), 0o400); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(stale, bytes.Repeat([]byte("B"), 70000), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := copyAttachment(context.Background(), profile, dir, "~/Files/a.bin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(stale); !bytes.Equal(got, bytes.Repeat([]byte("A"), 70000)) {
		t.Fatal("stale copy was kept")
	}
}

func TestCopyAttachmentStopsWhenCancelled(t *testing.T) {
	profile, dir := t.TempDir(), t.TempDir()
	src := filepath.Join(profile, "Files", "big.bin")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 1<<20), 0o400); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copyAttachment(ctx, profile, dir, "~/Files/big.bin"); err == nil {
		t.Fatal("copy went ahead after cancellation")
	}
	if _, err := os.Stat(filepath.Join(dir, "big.bin")); err == nil {
		t.Fatal("a cancelled copy left a file behind")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("temporary file left behind: %v", entries)
	}
}

func TestWriteAtomicWorksForANameAtTheLengthLimit(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("n", 255)
	if err := writeAtomic(filepath.Join(dir, name), func(w io.Writer) error { _, err := w.Write([]byte("ok")); return err }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, name)); string(got) != "ok" {
		t.Fatalf("content %q", got)
	}
}

func TestInsideDirComparesDirectoryIdentity(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "Profile")
	if err := os.MkdirAll(filepath.Join(profile, "Files"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(profile, link); err != nil {
		t.Fatal(err)
	}
	// a store entry that is itself a link out of the profile: the ENTRY is what a rename replaces
	if err := os.Symlink(filepath.Join(root, "elsewhere.hxd"), filepath.Join(profile, "HxStore.hxd")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		p    string
		want bool
	}{
		{filepath.Join(profile, "HxStore.hxd"), true},
		{filepath.Join(profile, "new", "deeper", "out.jsonl"), true},
		{filepath.Join(link, "out.jsonl"), true},
		{filepath.Join(profile, "x"), true},
		{filepath.Join(root, "export", "out.jsonl"), false},
		{filepath.Join(root, "Profile-2", "x"), false},
		{filepath.Join(root, "elsewhere.hxd"), false},
	}
	// a differently-cased alias on a case-insensitive volume names the same directory
	if _, err := os.Stat(filepath.Join(root, "profile")); err == nil {
		cases = append(cases, struct {
			p    string
			want bool
		}{filepath.Join(root, "profile", "HxStore.hxd"), true})
	}
	for _, c := range cases {
		got, err := insideDir(profile, c.p)
		if err != nil || got != c.want {
			t.Errorf("insideDir(%s) = %v, %v; want %v", c.p, got, err, c.want)
		}
	}
	// a relative profile path
	t.Chdir(profile)
	if got, err := insideDir(".", "HxStore.hxd"); err != nil || !got {
		t.Errorf("relative profile: %v, %v", got, err)
	}
}

func TestCopyAttachmentRefusesASymlinkedExportSubdirectory(t *testing.T) {
	root := t.TempDir()
	profile, dir := filepath.Join(root, "profile"), filepath.Join(root, "export")
	src := filepath.Join(profile, "Files", "S0", "a.txt")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("hello"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// export/S0 → <profile>/Files/S1: the copy would land inside the profile
	if err := os.MkdirAll(filepath.Join(profile, "Files", "S1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(profile, "Files", "S1"), filepath.Join(dir, "S0")); err != nil {
		t.Fatal(err)
	}
	if _, err := copyAttachment(context.Background(), profile, dir, "~/Files/S0/a.txt"); err == nil {
		t.Fatal("copy through the symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(profile, "Files", "S1", "a.txt")); err == nil {
		t.Fatal("a file was written inside the profile")
	}
	// export/S0 → somewhere else outside the export directory is refused too
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "S0")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, "S0")); err != nil {
		t.Fatal(err)
	}
	if _, err := copyAttachment(context.Background(), profile, dir, "~/Files/S0/a.txt"); err == nil {
		t.Fatal("copy leaving the export directory was accepted")
	}
}

func TestCanonPathClimbsFromALinkTarget(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	if err := os.MkdirAll(filepath.Join(profile, "Files"), 0o700); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(root, "export")
	if err := os.MkdirAll(export, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(profile, "Files"), filepath.Join(export, "link")); err != nil {
		t.Fatal(err)
	}
	realProfile, _ := filepath.EvalSymlinks(profile)
	// "export/link/../HxStore.hxd" is the profile's store to the kernel; filepath.Clean would say export/HxStore.hxd
	got, err := canonPath(filepath.Join(export, "link") + "/../HxStore.hxd")
	if err != nil || got != filepath.Join(realProfile, "HxStore.hxd") {
		t.Fatalf("canonPath = %s, %v", got, err)
	}
	if _, _, err := exportGuard(profile, filepath.Join(export, "link")+"/../HxStore.hxd", ""); err == nil {
		t.Fatal("-o through link/.. was accepted")
	}
	if _, _, err := exportGuard(profile, "", filepath.Join(export, "link")+"/.."); err == nil {
		t.Fatal("-attachments through link/.. was accepted")
	}
	// a profile named through link/.. (its Files dir is the link target) guards what it reads
	if _, _, err := exportGuard(filepath.Join(export, "link")+"/..", filepath.Join(profile, "HxStore.hxd"), ""); err == nil {
		t.Fatal("-o into a profile named through link/.. was accepted")
	}
	// a plain output file named x inside the profile is refused too
	t.Chdir(profile)
	if _, _, err := exportGuard(".", "x", ""); err == nil {
		t.Fatal("-o x inside the profile was accepted")
	}
	// and a legitimate destination comes back canonical, ready to write
	o, a, err := exportGuard(profile, filepath.Join(export, "out.jsonl"), filepath.Join(export, "files"))
	realExport, _ := filepath.EvalSymlinks(export)
	if err != nil || o != filepath.Join(realExport, "out.jsonl") || a != filepath.Join(realExport, "files") {
		t.Fatalf("guard: %s %s %v", o, a, err)
	}
}

func TestCopyAttachmentReplacesASymlinkAtTheDestination(t *testing.T) {
	root := t.TempDir()
	profile, dir := filepath.Join(root, "profile"), filepath.Join(root, "export")
	src := filepath.Join(profile, "Files", "a.txt")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("hello"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(src, filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	dst, err := copyAttachment(context.Background(), profile, dir, "~/Files/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dst)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("destination is still a link: %v %v", fi, err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "hello" {
		t.Fatalf("content %q", got)
	}
}
