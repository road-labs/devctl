package proc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Output goes to the file as well as the ring, and a second run appends rather
// than replacing: the record is meant to outlive one start.
func TestLogFileAppendsAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "svc.log")

	for _, word := range []string{"first", "second"} {
		p, err := Start(Spec{Command: "echo " + word, LogFile: path})
		require.NoError(t, err)
		waitForExit(t, p)
	}

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "first\nsecond\n", string(body), "the directory is created and the file appended")
}

// The cap holds while the process runs, not only when it starts. devctl runs
// all day and a service started this morning is the same process this evening,
// so a check that only ran at start would let one file grow without limit.
func TestLogFileRollsWhileRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svc.log")

	// Forty lines of ten bytes against a 100 byte cap: several rolls, not one.
	p, err := Start(Spec{
		Command:    "for i in $(seq 1 40); do echo 123456789; done",
		LogFile:    path,
		LogMaxSize: 100,
	})
	require.NoError(t, err)
	waitForExit(t, p)

	current, err := os.Stat(path)
	require.NoError(t, err)
	previous, err := os.Stat(path + ".1")
	require.NoError(t, err)

	assert.LessOrEqual(t, current.Size(), int64(100), "the live file stays under the cap")
	assert.LessOrEqual(t, previous.Size(), int64(100), "and so does the one generation kept")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "two files per row, for ever: the log and its predecessor")
}

// A file already over the cap rolls on the first line written to it, rather
// than being truncated: the log over the limit is usually the one about to be
// read, and a generation of history costs a rename.
func TestLogFileRollsWhatWasAlreadyOverTheCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svc.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 200)), 0o644))

	p, err := Start(Spec{Command: "echo fresh", LogFile: path, LogMaxSize: 100})
	require.NoError(t, err)
	waitForExit(t, p)

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "fresh\n", string(body), "the new file holds only this run")

	previous, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	assert.Len(t, previous, 200, "and the generation before it is kept")
}

// Under the cap, nothing moves.
func TestLogFileKeepsWhatIsUnderTheCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svc.log")
	require.NoError(t, os.WriteFile(path, []byte("small\n"), 0o644))

	p, err := Start(Spec{Command: "echo more", LogFile: path, LogMaxSize: 1 << 20})
	require.NoError(t, err)
	waitForExit(t, p)

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "small\nmore\n", string(body))
	assert.NoFileExists(t, path+".1")
}

func waitForExit(t *testing.T, p *Process) {
	t.Helper()
	require.Eventually(t, func() bool {
		state, _ := p.State()
		return state == StateExited
	}, 5*time.Second, 10*time.Millisecond)
}
