// Package peer is how one devctl reads another's live ports. A devctl with an
// id publishes its allocated ports on a unix socket in a well-known temp
// directory; a devctl that peers with it dials that socket by id and reads the
// number it needs. Ports are allocated once at start and do not move, so the
// published snapshot is fixed for the run and every read returns the same thing.
package peer

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// dialTimeout bounds both the liveness probe before publishing and every read.
// A sibling on the same machine answers immediately or not at all.
const dialTimeout = 300 * time.Millisecond

// Snapshot is what a devctl publishes: its id and the port it allocated for
// every service and port name, the same table services resolve each other
// through. A consumer picks the service and port it peers with.
type Snapshot struct {
	ID    string                    `json:"id"`
	Ports map[string]map[string]int `json:"ports"`
}

// Port returns the allocated number for a service's named port, or false when
// the sibling has no such listener.
func (s Snapshot) Port(service, port string) (int, bool) {
	n, ok := s.Ports[service][port]
	return n, ok
}

// dir is where sockets live: one directory under the system temp, per user
// already on the platforms devctl runs on.
func dir() string { return filepath.Join(os.TempDir(), "devctl") }

// socketPath is where the devctl with this id publishes.
func socketPath(id string) string { return filepath.Join(dir(), id+".sock") }

// SocketPath is where a devctl with this id publishes its ports, so the panel
// can tell a reader where a peered dependency is read from.
func SocketPath(id string) string { return socketPath(id) }

// Serve publishes the snapshot on this id's socket until the returned closer is
// closed. A socket already answering for the id is another running devctl, and
// that is refused rather than fought over; a leftover socket from a devctl that
// has gone is taken over.
func Serve(snap Snapshot) (io.Closer, error) {
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return nil, fmt.Errorf("peer: %w", err)
	}
	path := socketPath(snap.ID)
	if conn, err := net.DialTimeout("unix", path, dialTimeout); err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("id %q is already served by a running devctl", snap.ID)
	}
	// Not answering: a stale file from a devctl that exited without cleaning up,
	// or nothing at all. Either way it is ours to take.
	_ = os.Remove(path)

	body, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("peer: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("peer: publish %s: %w", path, err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write(body)
			_ = conn.Close()
		}
	}()
	return &server{ln: ln, path: path}, nil
}

// server closes the listener and removes the socket, so a devctl that quits
// leaves nothing for the next one to trip over.
type server struct {
	ln   net.Listener
	path string
}

func (s *server) Close() error {
	err := s.ln.Close()
	_ = os.Remove(s.path)
	return err
}

// Read returns the snapshot the devctl with this id is publishing. A socket that
// is missing or not answering is reported as not running, not an error: a
// sibling that has not started yet is an ordinary state, the same as a
// port-forward nobody has opened.
func Read(id string) (snap Snapshot, running bool, err error) {
	conn, err := net.DialTimeout("unix", socketPath(id), dialTimeout)
	if err != nil {
		return Snapshot{}, false, nil
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(dialTimeout))
	body, err := io.ReadAll(conn)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("peer: read %q: %w", id, err)
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		return Snapshot{}, false, fmt.Errorf("peer: %q sent something unreadable: %w", id, err)
	}
	return snap, true, nil
}
