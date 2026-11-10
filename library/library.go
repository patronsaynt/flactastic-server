// Package library is one user's music folder and its index: scanning,
// identity, hashing, and every change the server makes to files.
package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
	"flactastic/server/store"
)

const (
	StateScanning = "scanning"
	StateReady    = "ready"
	StateOffline  = "offline"
)

var ErrOffline = errors.New("library offline")

type Library struct {
	User    store.User
	Dir     string // the user's private data directory
	db      *sql.DB
	root    atomic.Pointer[Root]
	state   atomic.Value
	mu      sync.Mutex // serialises changes to files and the index
	wake    chan struct{}
	hashing chan struct{}
	stop    context.CancelFunc
	done    sync.WaitGroup

	changeMu sync.Mutex
	changed  chan struct{}

	sidecar      map[string]string // relative path → trackID, the Mac's track-ids.json
	sidecarDirty bool
}

// identity is <root>/.flactastic/library.json, the Mac's format.
type identity struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
}

// EnsureIdentity returns the library ID of root, creating library.json the
// first time.
func EnsureIdentity(root string) (string, error) {
	file := filepath.Join(root, ".flactastic", "library.json")
	if b, err := os.ReadFile(file); err == nil {
		var id identity
		if json.Unmarshal(b, &id) == nil && id.ID != "" {
			return strings.ToUpper(id.ID), nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	id := identity{Version: 1, ID: meta.NewUUID().String()}
	b, _ := json.Marshal(id)
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = f.Write(b)
	return id.ID, err
}

func Open(u store.User, dataDir string) (*Library, error) {
	dir := filepath.Join(dataDir, "users", u.ID)
	if err := os.MkdirAll(filepath.Join(dir, "art"), 0o750); err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(db, schema); err != nil {
		db.Close()
		return nil, err
	}
	l := &Library{User: u, Dir: dir, db: db, wake: make(chan struct{}, 1), hashing: make(chan struct{}, 1),
		changed: make(chan struct{})}
	l.state.Store(StateScanning)
	return l, nil
}

func (l *Library) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	l.stop = cancel
	l.done.Add(2)
	go func() { defer l.done.Done(); l.scanLoop(ctx) }()
	go func() { defer l.done.Done(); l.hashLoop(ctx) }()
}

func (l *Library) Close() {
	if l.stop != nil {
		l.stop()
		l.done.Wait()
	}
	l.db.Close()
}

func (l *Library) State() string { return l.state.Load().(string) }

func (l *Library) setState(s string) {
	if l.state.Swap(s) != s {
		l.write(func(*sql.Tx, int64) error { return nil }, KindLibrary)
	}
}

// Scan asks for an incremental scan soon.
func (l *Library) Scan() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *Library) notify() {
	l.changeMu.Lock()
	close(l.changed)
	l.changed = make(chan struct{})
	l.changeMu.Unlock()
}

// Wait blocks until something changes after since, ctx ends, or wait passes.
func (l *Library) Wait(ctx context.Context, since int64, wait time.Duration) (int64, []string) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		l.changeMu.Lock()
		ch := l.changed
		l.changeMu.Unlock()
		if seq, kinds := l.Changed(since); len(kinds) > 0 || wait <= 0 {
			return seq, kinds
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return l.Changed(since)
		case <-deadline.C:
			return l.Changed(since)
		}
	}
}

// Root returns the confined music folder, or ErrOffline.
func (l *Library) Root() (*Root, error) {
	if r := l.root.Load(); r != nil && l.State() != StateOffline {
		return r, nil
	}
	return nil, ErrOffline
}

// attach reopens the music folder and checks it is still the same library:
// a share that dropped leaves an empty mount point without library.json.
func (l *Library) attach() bool {
	r, err := openRoot(l.User.Root)
	if err == nil {
		var f *os.File
		if f, err = r.Open(".flactastic/library.json", unix.O_RDONLY, 0); err == nil {
			var id identity
			b, _ := io.ReadAll(io.LimitReader(f, 4096))
			f.Close()
			if json.Unmarshal(b, &id) != nil || !strings.EqualFold(id.ID, l.User.Library) {
				err = errors.New("library.json is missing or names another library")
			}
		}
	}
	if err != nil {
		if l.State() != StateOffline {
			slog.Warn("library offline", "user", l.User.Name, "root", l.User.Root, "err", err)
		}
		l.setState(StateOffline)
		return false
	}
	l.root.Store(r)
	return true
}

// Stats is the library summary (protocol §7).
type Stats struct {
	TrackCount, PendingHashes, UsedBytes int64
	FreeBytes                            uint64
}

func (l *Library) Stats() (s Stats) {
	l.db.QueryRow("SELECT count(*), count(*) - count(hash), coalesce(sum(size), 0) FROM tracks WHERE path IS NOT NULL").
		Scan(&s.TrackCount, &s.PendingHashes, &s.UsedBytes)
	var fs unix.Statfs_t
	if unix.Statfs(l.User.Root, &fs) == nil {
		s.FreeBytes = fs.Bavail * uint64(fs.Bsize)
	}
	return
}

// OpenTrack opens a track's file for reading, confined to the library.
func (l *Library) OpenTrack(t Track) (*os.File, error) {
	r, err := l.Root()
	if err != nil {
		return nil, err
	}
	return r.Open(t.RelativePath, unix.O_RDONLY, 0)
}
