package outlookdesktop

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepStaleRemovesOnlyOldSnapshots(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	mk := func(name string, age time.Duration) string {
		dir := filepath.Join(tmp, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "HxStore.hxd"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	stale := mk(snapshotPrefix+"stale", 2*time.Hour)
	fresh := mk(snapshotPrefix+"fresh", time.Minute)
	other := mk("someone-else-old", 2*time.Hour)
	if err := os.Symlink(other, filepath.Join(tmp, snapshotPrefix+"link")); err != nil {
		t.Fatal(err)
	}
	if n := SweepStale(time.Hour); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	for dir, want := range map[string]bool{stale: false, fresh: true, other: true} {
		if _, err := os.Stat(dir); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(dir), err == nil, want)
		}
	}
}
