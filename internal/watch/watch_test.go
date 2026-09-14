package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReportsGoChangesOncePerQuietPeriodAndIgnoresTheRest(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "svc", "node_modules"), 0o755))
	w, err := New(root, []string{"svc"}, 50*time.Millisecond)
	require.NoError(t, err)
	defer w.Close()

	// Two quick writes to Go files collapse into one report.
	require.NoError(t, os.WriteFile(filepath.Join(root, "svc", "a.go"), []byte("package svc"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "svc", "b.go"), []byte("package svc"), 0o600))
	select {
	case <-w.C:
	case <-time.After(2 * time.Second):
		t.Fatal("no change reported")
	}
	select {
	case p := <-w.C:
		t.Fatalf("second report for the same burst: %s", p)
	case <-time.After(150 * time.Millisecond):
	}

	// Files that are not Go source, and anything under node_modules, are ignored.
	require.NoError(t, os.WriteFile(filepath.Join(root, "svc", "notes.md"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "svc", "node_modules", "c.go"), []byte("x"), 0o600))
	select {
	case p := <-w.C:
		t.Fatalf("irrelevant change reported: %s", p)
	case <-time.After(150 * time.Millisecond):
	}

	// A directory created later is watched too.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "svc", "sub"), 0o755))
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(root, "svc", "sub", "d.go"), []byte("package sub"), 0o600))
	select {
	case <-w.C:
	case <-time.After(2 * time.Second):
		t.Fatal("change in a new directory not reported")
	}
}
