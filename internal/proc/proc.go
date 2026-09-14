// Package proc runs a service's command as a process group, keeps its recent
// output, and stops the whole group on request so that `go run` children do
// not outlive the tool.
package proc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Line is one captured line of output. Seq orders lines across processes:
// it is taken from one counter at capture time, so merging every process's
// tail by Seq reproduces the order things happened in.
type Line struct {
	Seq  uint64
	Text string
}

var sequence atomic.Uint64

// State is where a process is in its life.
type State int

const (
	StateRunning State = iota
	StateExited
)

// Spec is what to run.
type Spec struct {
	Command string
	Dir     string
	Env     map[string]string
}

// Process is a started command with a log ring.
type Process struct {
	cmd       *exec.Cmd
	done      chan struct{}
	logs      *ring
	startedAt time.Time

	mu       sync.Mutex
	state    State
	exitCode int
	exitErr  error
}

// Start launches the command through sh in its own process group. Output is
// captured line by line; the caller polls Tail.
func Start(spec Spec) (*Process, error) {
	cmd := exec.Command("sh", "-c", spec.Command)
	cmd.Dir = spec.Dir
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Own process group so Stop can signal `make`, `go run` and the compiled
	// child together.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	p := &Process{
		cmd:       cmd,
		done:      make(chan struct{}),
		logs:      newRing(2000),
		startedAt: time.Now(),
		state:     StateRunning,
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go p.capture(stdout, &readers)
	go p.capture(stderr, &readers)
	go func() {
		readers.Wait()
		err := cmd.Wait()
		p.mu.Lock()
		p.state = StateExited
		p.exitErr = err
		if cmd.ProcessState != nil {
			p.exitCode = cmd.ProcessState.ExitCode()
		}
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func (p *Process) capture(r interface{ Read([]byte) (int, error) }, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		p.logs.add(scanner.Text())
	}
}

// State returns the current state and, once exited, the exit code.
func (p *Process) State() (State, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state, p.exitCode
}

// PID returns the shell's process id, which is also the process group id.
func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Uptime is how long the process has been running.
func (p *Process) Uptime() time.Duration {
	return time.Since(p.startedAt).Truncate(time.Second)
}

// Tail returns the last n captured lines.
func (p *Process) Tail(n int) []string {
	lines := p.logs.tail(n)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

// TailLines returns the last n captured lines with their sequence numbers.
func (p *Process) TailLines(n int) []Line {
	return p.logs.tail(n)
}

// Stop signals the process group to terminate and kills it after timeout.
// It returns once the process has exited.
func (p *Process) Stop(timeout time.Duration) error {
	if state, _ := p.State(); state == StateExited {
		return nil
	}
	pgid := p.PID()
	if pgid == 0 {
		return nil
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-p.done:
		return nil
	case <-time.After(timeout):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("process group %d did not exit after SIGKILL", pgid)
	}
	return nil
}

// ring keeps the last N lines of output.
type ring struct {
	mu    sync.Mutex
	lines []Line
	start int
	count int
}

func newRing(capacity int) *ring {
	return &ring{lines: make([]Line, capacity)}
}

func (r *ring) add(text string) {
	line := Line{Seq: sequence.Add(1), Text: text}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count < len(r.lines) {
		r.lines[(r.start+r.count)%len(r.lines)] = line
		r.count++
		return
	}
	r.lines[r.start] = line
	r.start = (r.start + 1) % len(r.lines)
}

func (r *ring) tail(n int) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > r.count {
		n = r.count
	}
	out := make([]Line, 0, n)
	for i := r.count - n; i < r.count; i++ {
		out = append(out, r.lines[(r.start+i)%len(r.lines)])
	}
	return out
}

// Kill ends a process that is not ours to wait on, such as a leftover from
// an earlier devctl: SIGTERM to its process group first, SIGKILL after the
// timeout, and an error if it is still there afterwards.
func Kill(pid int, timeout time.Duration) error {
	target := -pid // the group, in case it is a `go run` with its binary
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		target = pid // not a group leader, signal just the process
	}
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminate pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(target, syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		return fmt.Errorf("pid %d did not exit after SIGKILL", pid)
	}
	return nil
}
