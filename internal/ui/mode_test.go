package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/road-labs/devctl/internal/peer"
)

// press feeds one key through Update, routed by whatever view is open, and
// returns the updated model.
func press(t *testing.T, m Model, s string) Model {
	t.Helper()
	var msg tea.KeyMsg
	switch s {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEscape}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	next, _ := m.Update(msg)
	return next.(Model)
}

func cursorOn(t *testing.T, m *Model, name string) {
	t.Helper()
	for i, r := range m.rows {
		if r.cfg.Name == name {
			m.cursor = i
			return
		}
	}
	t.Fatalf("%s is not a row", name)
}

// Choosing a mode re-resolves everything that reads the dependency, so the
// consumers point at the new source. The fixture's inventory is local (a sibling
// devctl) or staging (a forward).
func TestChoosingModeReresolvesConsumers(t *testing.T) {
	m := model(t, 140, 50)
	m.expand.Values["db.address"] = "postgres://localhost:5432/shop"
	inv := m.byName["inventory"]
	require.Equal(t, "local", inv.activeModeName, "the default mode is live at start")

	cat := m.byName["catalogue"]
	env, err := m.envFor(cat)
	require.NoError(t, err)
	assert.Empty(t, env["INVENTORY_ADDR"], "local peers with a sibling that is not running here")

	m.chooseMode(inv, "staging")
	assert.Equal(t, "staging", inv.activeModeName)

	env, err = m.envFor(cat)
	require.NoError(t, err)
	assert.Equal(t, "localhost:7110", env["INVENTORY_ADDR"],
		"the consumer now reads the staging forward's local port")
}

// The picker opens on a multi-mode dependency, starts on the live mode, and
// switches to whatever is chosen.
func TestModePickerOpensAndSwitches(t *testing.T) {
	m := model(t, 140, 50)
	m.expand.Values["db.address"] = "postgres://localhost:5432/shop"
	cursorOn(t, &m, "inventory")

	m = press(t, m, "m")
	require.True(t, m.modePick, "m opens the picker")
	assert.Equal(t, 0, m.modeCursor, "it starts on the live mode")

	m = press(t, m, "j")
	assert.Equal(t, 1, m.modeCursor, "j moves down the modes")

	m = press(t, m, "enter")
	assert.False(t, m.modePick, "enter closes the picker")
	assert.Equal(t, "staging", m.byName["inventory"].activeModeName, "and switches to the chosen mode")
}

// esc leaves the picker without changing anything.
func TestModePickerCancels(t *testing.T) {
	m := model(t, 140, 50)
	m.expand.Values["db.address"] = "postgres://localhost:5432/shop"
	cursorOn(t, &m, "inventory")

	m = press(t, m, "m")
	m = press(t, m, "j")
	m = press(t, m, "esc")
	assert.False(t, m.modePick)
	assert.Equal(t, "local", m.byName["inventory"].activeModeName, "cancel leaves the live mode alone")
}

// A digit jumps straight to a mode and chooses it.
func TestModePickerDigitJumps(t *testing.T) {
	m := model(t, 140, 50)
	m.expand.Values["db.address"] = "postgres://localhost:5432/shop"
	cursorOn(t, &m, "inventory")

	m = press(t, m, "m")
	m = press(t, m, "2")
	assert.False(t, m.modePick)
	assert.Equal(t, "staging", m.byName["inventory"].activeModeName, "2 chooses the second mode")
}

// A single-source dependency has no mode to switch, and says so rather than
// opening an empty picker.
func TestModePickerRefusesASingleSource(t *testing.T) {
	m := model(t, 140, 50)
	cursorOn(t, &m, "db")
	m = press(t, m, "m")
	assert.False(t, m.modePick)
	assert.Contains(t, m.message, "single source")
}

// A peered dependency waits for its sibling and picks it up when it appears,
// the same way a service follows a port allocated at start.
func TestPeerRowResolvesWhenSiblingAppears(t *testing.T) {
	m := model(t, 140, 50)
	m.expand.Values["db.address"] = "postgres://localhost:5432/shop"
	inv := m.byName["inventory"]
	require.Equal(t, "local", inv.activeModeName)
	assert.Empty(t, inv.address, "the warehouse sibling is not running yet")

	closer, err := peer.Serve(peer.Snapshot{ID: "warehouse", Ports: map[string]map[string]int{
		"stock": {"grpc": 24999},
	}})
	require.NoError(t, err)
	defer closer.Close()

	// One turn of what the probe does every half second.
	m.readPeer(inv)
	m.applyMode(inv)
	assert.Equal(t, "localhost:24999", inv.address, "the row follows the sibling's live port")

	env, err := m.envFor(m.byName["catalogue"])
	require.NoError(t, err)
	assert.Equal(t, "localhost:24999", env["INVENTORY_ADDR"], "and so does everything that reads it")
}
