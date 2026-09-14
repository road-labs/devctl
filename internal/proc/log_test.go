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

// A file already over the cap is rotated rather than truncated: the log over the
// limit is usually the one about to be read.
func TestLogFileRotatesWhenOverSize(t *testing.T) {
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
