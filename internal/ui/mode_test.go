package ui

import (
	"testing"
	"time"

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

// A service is handed the config its dependency's active mode provides, and
// switching the mode swaps that config along with the address.
func TestServiceInheritsModeProvides(t *testing.T) {
	m := model(t, 140, 50)
	m.expand.Values["db.address"] = "postgres://localhost:5432/shop"

	cat := m.byName["catalogue"] // depends_on inventory
	env, err := m.envFor(cat)
	require.NoError(t, err)
	assert.Equal(t, "local", env["STOCK_SOURCE"], "inherited from inventory's default mode")

	m.chooseMode(m.byName["inventory"], "staging")
	env, err = m.envFor(cat)
	require.NoError(t, err)
	assert.Equal(t, "staging", env["STOCK_SOURCE"], "switching the mode swaps the provided config")
}

// A service's own env wins over what a dependency provides, so a provided value
// is a default the service can still set for itself.
func TestServiceEnvOverridesProvides(t *testing.T) {
	m := model(t, 140, 50)
	m.expand.Values["db.address"] = "postgres://localhost:5432/shop"
	cat := m.byName["catalogue"]
	cat.cfg.Env["STOCK_SOURCE"] = "pinned"

	env, err := m.envFor(cat)
	require.NoError(t, err)
	assert.Equal(t, "pinned", env["STOCK_SOURCE"], "the service's own env beats the provided default")
}

// A peered dependency inherits the config the sibling service publishes, and the
// mode's own provides win over it.
func TestPeerPublishedProvidesInherited(t *testing.T) {
	closer, err := peer.Serve(peer.Snapshot{
		ID:       "warehouse",
		Ports:    map[string]map[string]int{"stock": {"grpc": 24999}},
		Provides: map[string]map[string]string{"stock": {"WAREHOUSE_REGION": "eu", "STOCK_SOURCE": "remote"}},
	})
	require.NoError(t, err)
	defer closer.Close()

	m := model(t, 140, 50)
	m.expand.Values["db.address"] = "postgres://localhost:5432/shop"
	inv := m.byName["inventory"]
	m.readPeer(inv)
	m.applyMode(inv)

	env, err := m.envFor(m.byName["catalogue"])
	require.NoError(t, err)
	assert.Equal(t, "eu", env["WAREHOUSE_REGION"], "inherited from the peered service's published config")
	assert.Equal(t, "local", env["STOCK_SOURCE"], "the mode's own provides win over the published base")
}

// A service that started before its peer resolved is restarted when the peer
// appears, so it picks up the address rather than staying on the empty one it
// booted with.
func TestPeerResolveRestartsRunningConsumer(t *testing.T) {
	m := model(t, 140, 50)
	m.root = t.TempDir()
	m.expand.Values["db.address"] = "postgres://x"

	// catalogue depends_on inventory (peered, sibling not up). Run it as a cheap
	// process so it is "running" when the peer resolves.
	cat := m.byName["catalogue"]
	cat.cfg.Cmd, cat.cfg.Dir, cat.cfg.Watch = "sleep 30", "", nil
	m.start(cat, map[string]bool{})
	deadline := time.Now().Add(3 * time.Second)
	for !cat.running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.True(t, cat.running(), "catalogue should be running")
	defer m.stopAll()

	starts := cat.starts
	// The probe reports the warehouse peer resolving.
	next, _ := m.Update(probeMsg{
		ports: map[string]map[int]bool{}, deps: map[string]bool{},
		peers: map[string]peerRead{"inventory": {port: 24999, provides: map[string]string{"STOCK_SOURCE": "local"}}},
	})
	m = next.(Model)
	assert.Greater(t, cat.starts, starts, "the running consumer restarted when the peer resolved")

	// A steady probe (nothing changed) must not churn it.
	steady := cat.starts
	next, _ = m.Update(probeMsg{
		ports: map[string]map[int]bool{}, deps: map[string]bool{},
		peers: map[string]peerRead{"inventory": {port: 24999, provides: map[string]string{"STOCK_SOURCE": "local"}}},
	})
	m = next.(Model)
	assert.Equal(t, steady, cat.starts, "an unchanged peer read does not restart anything")
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
