package peer

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// id keeps each test on its own socket, since the registry is a directory
// shared by every devctl on the machine.
func id(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("devctl-test-%d-%d", os.Getpid(), time.Now().UnixNano())
}

func TestServeReadRoundTrip(t *testing.T) {
	name := id(t)
	closer, err := Serve(Snapshot{
		ID:       name,
		Ports:    map[string]map[string]int{"stock": {"grpc": 24999, "http": 24998}},
		Provides: map[string]map[string]string{"stock": {"WAREHOUSE_REGION": "eu"}},
	})
	require.NoError(t, err)
	defer closer.Close()

	snap, running, err := Read(name)
	require.NoError(t, err)
	require.True(t, running, "a served id reads as running")
	assert.Equal(t, name, snap.ID)

	port, ok := snap.Port("stock", "grpc")
	require.True(t, ok)
	assert.Equal(t, 24999, port, "the reader follows the number the sibling actually got")

	assert.Equal(t, map[string]string{"WAREHOUSE_REGION": "eu"}, snap.ProvidesFor("stock"),
		"a peered service's published config comes across too")
	assert.Nil(t, snap.ProvidesFor("nothing"), "a service that publishes none has none")

	_, ok = snap.Port("stock", "nope")
	assert.False(t, ok, "an unknown listener is reported as absent, not zero")

	// Ports do not move, so every read returns the same snapshot.
	again, running, err := Read(name)
	require.NoError(t, err)
	require.True(t, running)
	assert.Equal(t, snap, again)
}

func TestReadMissingIsNotRunning(t *testing.T) {
	snap, running, err := Read(id(t))
	require.NoError(t, err, "a sibling that never started is an ordinary state, not an error")
	assert.False(t, running)
	assert.Zero(t, snap.ID)
}

func TestServeRefusesADuplicateID(t *testing.T) {
	name := id(t)
	first, err := Serve(Snapshot{ID: name})
	require.NoError(t, err)
	defer first.Close()

	_, err = Serve(Snapshot{ID: name})
	require.Error(t, err, "a second devctl on the same id is refused rather than fought over")
	assert.Contains(t, err.Error(), name)
}

func TestServeTakesOverAStaleSocket(t *testing.T) {
	name := id(t)
	// A file left where the socket goes, with nothing listening: a devctl that
	// exited without cleaning up. The next one takes it over rather than failing.
	require.NoError(t, os.MkdirAll(dir(), 0o700))
	require.NoError(t, os.WriteFile(socketPath(name), []byte("stale"), 0o600))

	closer, err := Serve(Snapshot{ID: name})
	require.NoError(t, err)
	defer closer.Close()

	_, running, err := Read(name)
	require.NoError(t, err)
	assert.True(t, running, "the new devctl answers on the reclaimed socket")
}

func TestCloseRemovesTheSocket(t *testing.T) {
	name := id(t)
	closer, err := Serve(Snapshot{ID: name})
	require.NoError(t, err)
	require.NoError(t, closer.Close())

	_, err = net.DialTimeout("unix", socketPath(name), dialTimeout)
	assert.Error(t, err, "a devctl that quit leaves nothing behind")
}
