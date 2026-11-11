package library

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"flactastic/server/store"
)

// Manager runs one Library per user and follows changes the CLI makes to
// server.db: new users, removed users, moved roots, scan requests.
type Manager struct {
	Store *store.Store
	mu    sync.Mutex
	libs  map[string]*Library
}

func NewManager(s *store.Store) *Manager { return &Manager{Store: s, libs: map[string]*Library{}} }

// Get returns a user's library. The ID comes from an authenticated token,
// never from a request, so one user can't name another's library.
func (m *Manager) Get(userID string) *Library {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.libs[userID]
}

func (m *Manager) All() []*Library {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Library, 0, len(m.libs))
	for _, l := range m.libs {
		out = append(out, l)
	}
	return out
}

func (m *Manager) Run(ctx context.Context) {
	for {
		m.sync()
		for _, r := range m.Store.TakeRequests() {
			if l := m.Get(r[1]); l != nil {
				if r[0] == "rebuild" {
					l.db.Exec("UPDATE tracks SET mtime = 0")
				}
				l.report.Store(true)
				l.Scan()
			}
		}
		select {
		case <-ctx.Done():
			for _, l := range m.All() {
				l.Close()
			}
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (m *Manager) sync() {
	users, err := m.Store.Users()
	if err != nil {
		slog.Error("reading users", "err", err)
		return
	}
	want := map[string]store.User{}
	for _, u := range users {
		want[u.ID] = u
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A library's User never changes while it runs: any edit restarts it.
	for id, l := range m.libs {
		if u, ok := want[id]; !ok || !sameUser(u, l.User) {
			l.Close()
			delete(m.libs, id)
		}
	}
	for id, u := range want {
		if m.libs[id] != nil {
			continue
		}
		l, err := Open(u, m.Store.Dir)
		if err != nil {
			slog.Error("opening library", "user", u.Name, "err", err)
			continue
		}
		l.Start()
		m.libs[id] = l
	}
}

func sameUser(a, b store.User) bool {
	q := func(p *int64) int64 {
		if p == nil {
			return -1
		}
		return *p
	}
	return a.Name == b.Name && a.Root == b.Root && a.Library == b.Library && a.Admin == b.Admin && q(a.Quota) == q(b.Quota)
}
