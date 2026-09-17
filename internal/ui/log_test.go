package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/road-labs/devctl/internal/proc"
)

// A line wider than the terminal is cut at its edge, which for a JSON log line
// is most of it. Folded, every piece fits, and in the merged view the
// continuations sit under the text rather than under the name column.
func TestWrapLineFoldsUnderTheText(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("word ", 12)) // 59 columns
	assert.Equal(t, text, wrapLine(text, 80, 0), "a line that fits is left alone")

	for _, l := range strings.Split(wrapLine(text, 30, 0), "\n") {
		assert.LessOrEqual(t, lipgloss.Width(l), 30, "%q", l)
	}

	lines := strings.Split(wrapLine(text, 30, 10), "\n")
	require.Greater(t, len(lines), 1, "59 columns into 20 folds")
	for i, l := range lines {
		assert.LessOrEqual(t, lipgloss.Width(l), 30, "%q", l)
		if i > 0 {
			assert.True(t, strings.HasPrefix(l, strings.Repeat(" ", 10)), "continuation %d sits under the text: %q", i, l)
		}
	}

	assert.Equal(t, text, wrapLine(text, 12, 10), "no room to fold into leaves the line to the viewport")
}

// w toggles wrapping in the log view, the title says so, and the setting
// outlives the view: it is a preference, not something to set again per row.
func TestLogViewWrapToggles(t *testing.T) {
	m := model(t, 140, 50)
	m = press(t, m, "L")
	require.True(t, m.logView)
	assert.False(t, m.wrap, "cut at the edge until asked")
	assert.NotContains(t, m.View(), "wrapped")

	m = press(t, m, "w")
	assert.True(t, m.wrap)
	assert.Contains(t, m.View(), "wrapped")

	m = press(t, m, "esc")
	require.False(t, m.logView)
	m = press(t, m, "l")
	assert.True(t, m.wrap, "the setting outlives the view")

	m = press(t, m, "w")
	assert.False(t, m.wrap)
}

// A running row's long line reaches the screen whole once wrapped, where
// unwrapped the viewport shows only what fits beside the name.
func TestWrappedLogsShowTheWholeLine(t *testing.T) {
	m := model(t, 60, 30)
	const token, repeats = "abcdefghij", 20
	line := strings.TrimSpace(strings.Repeat(token+" ", repeats))
	p, err := proc.Start(proc.Spec{Command: `printf '%s\n' "` + line + `"`})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Stop(time.Second) })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(p.Tail(1)) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	require.NotEmpty(t, p.Tail(1), "the process printed its line")
	m.byName["catalogue"].proc = p

	m = press(t, m, "L")
	assert.Less(t, strings.Count(m.View(), token), repeats, "cut at the edge, most of the line is off screen")

	m = press(t, m, "w")
	assert.Equal(t, repeats, strings.Count(m.View(), token), "wrapped, all of it is on screen")
}
