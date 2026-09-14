// Package watch notices source changes under a set of directories and
// reports them once per quiet period, so a service can be restarted after a
// save rather than after every keystroke of a multi-file write.
package watch

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher reports changes on C; the value is a sample changed path.
type Watcher struct {
	C    chan string
	fs   *fsnotify.Watcher
	done chan struct{}
}

// skipped are directory names never watched: build output and dependencies.
var skipped = map[string]bool{".git": true, "node_modules": true, ".next": true, "dist": true, "vendor": true, "bin": true}

// New watches every directory under dirs (relative to root) recursively and
// reports Go source, go.mod and go.sum changes after debounce of quiet.
func New(root string, dirs []string, debounce time.Duration) (*Watcher, error) {
	fs, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{C: make(chan string, 1), fs: fs, done: make(chan struct{})}
	for _, dir := range dirs {
		if err := w.addTree(filepath.Join(root, dir)); err != nil {
			_ = fs.Close()
			return nil, err
		}
	}
	go w.run(debounce)
	return w, nil
}

func (w *Watcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if skipped[d.Name()] && path != dir {
			return filepath.SkipDir
		}
		return w.fs.Add(path)
	})
}

// relevant reports whether a change to path should restart anything.
func relevant(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, ".go") || base == "go.mod" || base == "go.sum"
}

func (w *Watcher) run(debounce time.Duration) {
	var timer *time.Timer
	var pending string
	var fire <-chan time.Time
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fs.Events:
			if !ok {
				return
			}
			// A new directory joins the watch so files created in it count.
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() && !skipped[filepath.Base(ev.Name)] {
					_ = w.addTree(ev.Name)
				}
			}
			if !relevant(ev.Name) {
				continue
			}
			pending = ev.Name
			if timer == nil {
				timer = time.NewTimer(debounce)
			} else {
				timer.Reset(debounce)
			}
			fire = timer.C
		case <-fire:
			fire = nil
			select {
			case w.C <- pending:
			default: // a change is already waiting to be consumed
			}
		case _, ok := <-w.fs.Errors:
			if !ok {
				return
			}
		}
	}
}

// Close stops watching.
func (w *Watcher) Close() error {
	close(w.done)
	return w.fs.Close()
}
