package workers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/registry"
)

// DefaultMaxWorkers is the cap on live workers per repo when settings.json sets no max_workers.
const DefaultMaxWorkers = 1

// MaxWorkers is settings.json's "max_workers", or DefaultMaxWorkers.
func MaxWorkers() int {
	home, err := os.UserHomeDir()
	if err != nil {
		return DefaultMaxWorkers
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return DefaultMaxWorkers
	}
	var settings struct {
		MaxWorkers *int `json:"max_workers"`
	}
	if json.Unmarshal(data, &settings) != nil || settings.MaxWorkers == nil {
		return DefaultMaxWorkers
	}
	return *settings.MaxWorkers
}

// StartLock serialises count-then-spawn across every TUI on the machine.
func StartLock() (func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, paths.State, "workers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "start.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// AllocOneoff makes the first free WORKER-N channel directory under root and returns its key.
func AllocOneoff(root string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	for n := 1; ; n++ {
		key := fmt.Sprintf("WORKER-%d", n)
		err := os.Mkdir(filepath.Join(root, key), 0o700)
		if err == nil {
			return key, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
}

// Archive moves key's channel directory under root/.archive, which the ledger does not scan.
func Archive(root, key string) error {
	dir := filepath.Join(root, key)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	aside := filepath.Join(root, ".archive")
	if err := os.MkdirAll(aside, 0o700); err != nil {
		return err
	}
	return os.Rename(dir, filepath.Join(aside, fmt.Sprintf("%s-%d", key, time.Now().UnixNano())))
}

// Starting is the worker.json of key whose process is alive, as a worker still claiming its slot has no registry entry yet.
func Starting(key string) (Meta, bool) {
	dir, err := Dir(key)
	if err != nil {
		return Meta{}, false
	}
	var m Meta
	if ReadJSON(filepath.Join(dir, "worker.json"), &m) != nil || !registry.Alive(m.PID) {
		return Meta{}, false
	}
	return m, true
}

// LiveKey says key has a live process in any repo.
func LiveKey(key string) bool {
	for _, info := range registry.Default("", "").Others() {
		if info.Worker == key {
			return true
		}
	}
	_, ok := Starting(key)
	return ok
}

// Live is the keys of origin's live, non-crashed workers, counting those still starting.
func Live(origin string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, t := range Read(origin) {
		if t.State != Crashed && t.State != Failed && t.State != Done {
			keys, seen[t.Key] = append(keys, t.Key), true
		}
	}
	root, err := Dir("")
	if err != nil {
		return keys
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() || seen[e.Name()] {
			continue
		}
		if m, ok := Starting(e.Name()); ok && m.Origin == origin {
			keys = append(keys, e.Name())
		}
	}
	return keys
}
