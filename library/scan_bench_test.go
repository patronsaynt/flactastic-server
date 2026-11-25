package library

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"flactastic/server/store"
)

// BenchmarkRescanUnchanged measures a scan that finds nothing new: the cost
// every inotify burst pays.
func BenchmarkRescanUnchanged(b *testing.B) {
	src, err := os.ReadFile("../testdata/audio/cur-single.flac")
	if err != nil {
		b.Fatal(err)
	}
	root, data := b.TempDir(), b.TempDir()
	old := time.Now().Add(-time.Hour)
	for i := 0; i < 5000; i++ {
		dir := filepath.Join(root, fmt.Sprintf("Artist %d", i/50), fmt.Sprintf("Album %d", i/5))
		os.MkdirAll(dir, 0o755)
		f := filepath.Join(dir, fmt.Sprintf("%02d.flac", i%5))
		os.WriteFile(f, src, 0o644)
		os.Chtimes(f, old, old) // past the scanner's settle time
	}
	id, _ := EnsureIdentity(root)
	l, err := Open(store.User{ID: "bench", Name: "bench", Root: root, Library: id}, data)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	if !l.attach() {
		b.Fatal("attach failed")
	}
	l.scan(-1)
	var n int
	l.db.QueryRow("SELECT COUNT(*) FROM tracks WHERE path IS NOT NULL").Scan(&n)
	if n != 5000 {
		b.Fatalf("%d tracks indexed", n)
	}
	b.ResetTimer()
	for b.Loop() {
		l.scan(-1)
	}
}
