package proc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartCapturesOutputAndStopKillsTheGroup(t *testing.T) {
	// A shell that spawns a child and waits: Stop must take the child down too.
	p, err := Start(Spec{Command: `echo hello; sh -c 'sleep 30' & wait`, Env: map[string]string{"X": "1"}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Stop(time.Second) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(p.Tail(10)) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.Equal(t, []string{"hello"}, p.Tail(10))

	state, _ := p.State()
	assert.Equal(t, StateRunning, state)

	start := time.Now()
	require.NoError(t, p.Stop(2*time.Second))
	assert.Less(t, time.Since(start), 3*time.Second, "stop must not wait for the 30s sleep")

	state, _ = p.State()
	assert.Equal(t, StateExited, state)
}

func TestExitCodeIsReported(t *testing.T) {
	p, err := Start(Spec{Command: `echo oops >&2; exit 3`})
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if state, _ := p.State(); state == StateExited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	state, code := p.State()
	assert.Equal(t, StateExited, state)
	assert.Equal(t, 3, code)
	assert.Equal(t, []string{"oops"}, p.Tail(10), "stderr is captured too")
}

func TestRingKeepsTheLastLines(t *testing.T) {
	r := newRing(3)
	for _, l := range []string{"a", "b", "c", "d", "e"} {
		r.add(l)
	}
	assert.Equal(t, []string{"c", "d", "e"}, texts(r.tail(10)))
	assert.Equal(t, []string{"e"}, texts(r.tail(1)))
	assert.Empty(t, r.tail(0))
}

func TestSequenceOrdersLinesAcrossRings(t *testing.T) {
	a, b := newRing(10), newRing(10)
	a.add("a1")
	b.add("b1")
	a.add("a2")
	lines := append(a.tail(10), b.tail(10)...)
	seqs := make([]uint64, 0, len(lines))
	for _, l := range lines {
		seqs = append(seqs, l.Seq)
	}
	// a1 < b1 < a2 by capture order, whatever ring they sit in.
	if seqs[0] >= seqs[2] || seqs[2] >= seqs[1] {
		t.Fatalf("sequence does not follow capture order: %v", seqs)
	}
}

func texts(lines []Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}
