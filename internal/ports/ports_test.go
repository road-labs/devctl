package ports

import (
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/road-labs/devctl/internal/config"
)

func manifest(t *testing.T) *config.File {
	t.Helper()
	file, err := config.Load(filepath.Join("..", "..", "testdata", "devctl.yaml"))
	require.NoError(t, err)
	return file
}

func TestAllocateKeepsFreeDefaults(t *testing.T) {
	table, warnings, err := Allocate(manifest(t), func(int) bool { return true }, func() (int, error) { return 0, errors.New("unexpected") })
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.Equal(t, 7200, table["catalogue"]["http"])
	assert.Equal(t, 7300, table["storefront"]["web"])
	assert.Equal(t, 7100, table["payments"][ForwardPort], "forwarded dependencies get their local port from the same table")
}

func TestAllocateMovesABusyDefaultAndReportsIt(t *testing.T) {
	busy := func(port int) bool { return port != 7201 }
	next := 40000
	pick := func() (int, error) { next++; return next, nil }

	table, warnings, err := Allocate(manifest(t), busy, pick)
	require.NoError(t, err)
	assert.Equal(t, 40001, table["catalogue"]["grpc"])
	// The warning may also name the process holding the port on this machine.
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "catalogue.grpc: default 7201 in use")
	assert.Contains(t, warnings[0], "using 40001")

	// Everyone who refers to catalogue.grpc follows the move.
	got, err := table.Expand("{{ catalogue.grpc }} {{ catalogue.grpc.number }} {{ catalogue.http.url }}")
	require.NoError(t, err)
	assert.Equal(t, "localhost:40001 40001 http://localhost:7200", got)
}

func TestAllocateRefusesToMoveFixedPorts(t *testing.T) {
	for _, tc := range []struct {
		port int
		want string
	}{{7200, "catalogue.http"}, {7300, "storefront.web"}} {
		busy := func(port int) bool { return port != tc.port }
		_, _, err := Allocate(manifest(t), busy, func() (int, error) { return 40001, nil })
		require.Error(t, err)
		assert.Contains(t, err.Error(), tc.want)
		assert.Contains(t, err.Error(), "fixed")
	}
}

// A pinned port is for something written down outside this repository, and it
// stops the run rather than moving. Everything else moves out of the way of
// whatever already holds its default, and says so.
func TestOnlyPinnedPortsRefuseToMove(t *testing.T) {
	catalogue := mustService(t, "catalogue")
	for _, name := range []string{"grpc", "metrics"} {
		port, ok := catalogue.Port(name)
		require.True(t, ok, name)
		require.False(t, port.Fixed, name)

		busy := func(p int) bool { return p != port.Number }
		table, warnings, err := Allocate(manifest(t), busy, func() (int, error) { return 40001, nil })
		require.NoError(t, err, "catalogue.%s is not pinned, so it may move", name)
		assert.Equal(t, 40001, table["catalogue"][name])
		assert.Contains(t, strings.Join(warnings, "\n"), "catalogue."+name)
	}
}

func mustService(t *testing.T, name string) config.Service {
	t.Helper()
	for _, svc := range manifest(t).Services {
		if svc.Name == name {
			return svc
		}
	}
	t.Fatalf("%s is not in the example manifest", name)
	return config.Service{}
}

func TestAllocateRejectsDuplicateDefaults(t *testing.T) {
	file := &config.File{Services: []config.Service{
		{Name: "a", Ports: []config.Port{{Name: "grpc", Number: 9000}}, Cmd: "x"},
		{Name: "b", Ports: []config.Port{{Name: "grpc", Number: 9000}}, Cmd: "x"},
	}}
	_, _, err := Allocate(file, func(int) bool { return true }, PickFree)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both declare port 9000")
}

func TestExpandRejectsUnknownReferences(t *testing.T) {
	table := Table{"identity": {"auth": 9093}}
	_, err := table.Expand("{{ identity.nope }}")
	assert.Error(t, err)
	_, err = table.Expand("{{ ghost.auth }}")
	assert.Error(t, err)
	plain, err := table.Expand("no references here")
	require.NoError(t, err)
	assert.Equal(t, "no references here", plain)
}

func TestValidateCatchesManifestMistakes(t *testing.T) {
	require.NoError(t, Validate(manifest(t)), "the real manifest must be internally consistent")

	badRef := &config.File{Services: []config.Service{{
		Name: "a", Ports: []config.Port{{Name: "grpc", Number: 9000}},
		Cmd: "x", Env: map[string]string{"DEP": "{{ b.grpc }}"},
	}}}
	assert.Error(t, Validate(badRef))

	// A mode's provides and a profile's env are handed to services, so an
	// unresolved reference in either is caught at load too.
	badProvides := &config.File{
		Dependencies: []config.Dependency{{Name: "d", Peer: &config.Peer{ID: "x", Service: "s", Port: "p"}, Provides: map[string]string{"V": "{{ ghost.grpc }}"}}},
		Services:     []config.Service{{Name: "a", Cmd: "x"}},
	}
	assert.Error(t, Validate(badProvides), "a bad reference in provides")

	badProfileEnv := &config.File{
		Profiles: []config.Profile{{Name: "p", Include: []string{"a"}, Env: map[string]string{"V": "{{ ghost.grpc }}"}}},
		Services: []config.Service{{Name: "a", Cmd: "x"}},
	}
	assert.Error(t, Validate(badProfileEnv), "a bad reference in a profile's env")
}

func TestIsFreeAndPickFreeAgree(t *testing.T) {
	port, err := PickFree()
	require.NoError(t, err)
	assert.True(t, IsFree(port))
}

func TestHolderNamesTheListeningProcess(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	holder := Holder(port)
	if holder == "" {
		t.Skip("lsof not available or not permitted here")
	}
	assert.Contains(t, holder, "pid ", "the test binary itself holds the port")
	assert.Equal(t, "", Holder(1), "nothing listens on port 1")
}

func TestIsFreeSeesWildcardListeners(t *testing.T) {
	// A wildcard listener, as Next dev binds, must count as holding the port
	// even though a loopback bind would still succeed on macOS.
	wildcard, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer wildcard.Close()
	assert.False(t, IsFree(wildcard.Addr().(*net.TCPAddr).Port))

	loopback, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer loopback.Close()
	assert.False(t, IsFree(loopback.Addr().(*net.TCPAddr).Port))

	free, err := PickFree()
	require.NoError(t, err)
	assert.True(t, IsFree(free))
}
