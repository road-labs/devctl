// Package proc runs a service's command as a process group, keeps its recent
// output, and stops the whole group on request so that `go run` children do
// not outlive the tool.
package proc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// LogFile, when set, receives every captured line as well as the ring. It
	// is opened for append, so a restart continues the same file and the record
	// outlives the panel.
	LogFile string
	// LogMaxSize bounds LogFile: when a write would take it past this many
	// bytes the file moves to <name>.1 and a new one begins, so the pair is
	// never more than twice this. Zero never rotates and grows without limit.
	LogMaxSize int64
}

// Process is a started command with a log ring.
type Process struct {
	cmd       *exec.Cmd
	done      chan struct{}
	logs      *ring
	startedAt time.Time
	logPath   string

	// logMu guards the file: two capture goroutines write to it.
	logMu   sync.Mutex
	logFile *os.File
	logMax  int64
	logSize int64

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
	// Opened before the command, so a directory that cannot be written is an
	// error the caller sees rather than output quietly going nowhere.
	logFile, logSize, err := openLog(spec.LogFile)
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("start: %w", err)
	}

	p := &Process{
		cmd:       cmd,
		done:      make(chan struct{}),
		logs:      newRing(2000),
		startedAt: time.Now(),
		state:     StateRunning,
		logPath:   spec.LogFile,
		logFile:   logFile,
		logMax:    spec.LogMaxSize,
		logSize:   logSize,
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
		p.logMu.Lock()
		if p.logFile != nil {
			_ = p.logFile.Close()
			p.logFile = nil
		}
		p.logMu.Unlock()
		close(p.done)
	}()
	return p, nil
}

// openLog prepares a service's log file: the directory, the handle, and how
// much is already in it. The size matters because the cap is enforced on every
// write, not at start: devctl runs all day, a service started this morning is
// still the same process this evening, and a check that only ran at start would
// let one file grow without limit while appearing to be capped.
func openLog(path string) (*os.File, int64, error) {
	if path == "" {
		return nil, 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, 0, fmt.Errorf("log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, 0, fmt.Errorf("open log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat log: %w", err)
	}
	return f, info.Size(), nil
}

// appendLog writes one line, rolling the file over first when it is full. A
// row therefore occupies at most twice the cap on disk, for as long as devctl
// runs, which is the point: a development log directory that keeps growing is
// one somebody eventually has to remember to delete.
func (p *Process) appendLog(line string) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	if p.logFile == nil {
		return
	}
	if p.logMax > 0 && p.logSize+int64(len(line))+1 > p.logMax {
		p.roll()
		if p.logFile == nil {
			return
		}
	}
	n, err := p.logFile.WriteString(line + "\n")
	if err != nil {
		// A log that cannot be written must not take the run down with it, and
		// the ring still has the line.
		_ = p.logFile.Close()
		p.logFile = nil
		return
	}
	p.logSize += int64(n)
}

// roll moves the full file aside and begins a new one, keeping exactly one
// generation. The caller holds logMu.
func (p *Process) roll() {
	_ = p.logFile.Close()
	p.logFile = nil
	if err := os.Rename(p.logPath, p.logPath+".1"); err != nil {
		return
	}
	f, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	p.logFile, p.logSize = f, 0
}

// LogPath is where this process's output is being appended, or "" when the
// manifest declares no log directory.
func (p *Process) LogPath() string { return p.logPath }

func (p *Process) capture(r interface{ Read([]byte) (int, error) }, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		p.logs.add(line)
		p.appendLog(line)
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
