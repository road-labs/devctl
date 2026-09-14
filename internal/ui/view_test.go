package ui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"github.com/road-labs/devctl/internal/config"
	"github.com/road-labs/devctl/internal/ports"
)

// model builds a panel over the shared fixture, with every port allocated at
// its declared number and nothing running.
func model(t *testing.T, width, height int) Model {
	t.Helper()
	file, err := config.Load(filepath.Join("..", "..", "testdata", "devctl.yaml"))
	require.NoError(t, err)
	table, warnings, err := ports.Allocate(file, func(int) bool { return true }, func() (int, error) { return 0, nil })
	require.NoError(t, err)
	m := New("/home/dev/shop", file, table, ports.Values{"DATABASE_URL": "postgres://localhost:5432/shop"}, warnings)
	m.width, m.height = width, height
	return m
}

// The panel is drawn by padding text to a width, so a line that measures wider
// than the terminal means something was counted in bytes rather than columns,
// and the box will be ragged.
func TestViewFitsItsWidth(t *testing.T) {
	for _, width := range []int{100, 120, 160, 200} {
		m := model(t, width, 50)
		for i, line := range strings.Split(m.View(), "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), width, "line %d at width %d: %q", i, width, line)
		}
	}
}

// The panel closes, so a missing bottom rule shows up as a test failure rather
// than as a smear down the terminal.
func TestViewDrawsAClosedPanel(t *testing.T) {
	out := model(t, 140, 50).View()
	require.Equal(t, strings.Count(out, "╭"), strings.Count(out, "╰"), "one bottom rule per top rule")
	require.Contains(t, out, "shop[6]", "the title counts the things, not the listeners under them")
}

// One table holds all three kinds, and the TYPE column is what tells them apart
// now that they no longer have a panel each.
func TestViewListsEveryKind(t *testing.T) {
	out := model(t, 140, 50).View()
	for _, want := range []string{"tcp", "forward", "service", "task"} {
		require.Contains(t, out, want)
	}
	require.Contains(t, out, "RESTARTS")
	require.Contains(t, out, "WATCH", "auto-restart reads as a setting, not a glyph beside the name")
}

// A rule is drawn where one band ends, and page up and page down step between
// the bands rather than by a screenful, which a list this short does not need.
func TestGroupsAreSeparatedAndNavigable(t *testing.T) {
	m := model(t, 140, 50)
	require.Contains(t, m.View(), strings.Repeat("─", 20), "a rule between the bands")

	// The fixture is two dependencies, three services, one task, in that order.
	require.Equal(t, 0, m.rows[m.cursor].group(), "the cursor starts on a dependency")
	m.cursor = m.nextGroup(1)
	require.Equal(t, 1, m.rows[m.cursor].group(), "down to the services")
	m.cursor = m.nextGroup(1)
	require.Equal(t, 2, m.rows[m.cursor].group(), "down to the tasks")
	m.cursor = m.nextGroup(1)
	require.Equal(t, 2, m.rows[m.cursor].group(), "the last band holds the cursor")
	m.cursor = m.nextGroup(-1)
	require.Equal(t, 1, m.rows[m.cursor].group(), "back up to the services")
	m.cursor = m.nextGroup(-1)
	require.Equal(t, 0, m.rows[m.cursor].group(), "and to the dependencies")
	require.Equal(t, 0, m.nextGroup(-1), "the first band holds it at the top")
}

// Describe answers the questions the table has no room for, including which way
// the dependency edges point.
func TestDescribeShowsTheGraph(t *testing.T) {
	m := model(t, 140, 50)
	for i, r := range m.rows {
		if r.cfg.Name == "catalogue" {
			m.cursor = i
		}
	}
	out := m.describeBody()
	require.Contains(t, out, "listeners")
	require.Contains(t, out, "depends on")
	require.Contains(t, out, "needed by")
	require.Contains(t, out, "storefront", "the reverse edge, which the manifest never states")
}
