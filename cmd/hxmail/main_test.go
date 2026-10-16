package main

import (
	"bytes"
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
	dst, err := copyAttachment(profile, dir, "~/Files/S0/3/Attachments/0/report[65].pdf")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "S0", "3", "Attachments", "0", "report[65].pdf")
	if dst != want {
		t.Fatalf("copy at %s, want %s", dst, want)
	}
	if got, _ := os.ReadFile(dst); len(got) != 300 {
		t.Fatalf("copied %d bytes", len(got))
	}
	// a second run keeps the existing copy
	if again, err := copyAttachment(profile, dir, "~/Files/S0/3/Attachments/0/report[65].pdf"); err != nil || again != dst {
		t.Fatalf("second copy: %s %v", again, err)
	}
	for _, bad := range []string{"~/Files/../HxStore.hxd", "/etc/passwd", "Files/S0/3/x", "~/Files/S0/../../../x"} {
		if _, err := copyAttachment(profile, dir, bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(entries) != 1 || !strings.HasSuffix(entries[0], "S0") {
		t.Fatalf("unexpected files in the copy dir: %v", entries)
	}
}
