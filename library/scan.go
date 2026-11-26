package library

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
	"flactastic/server/taglib"
)

const (
	xattrName  = "user.com.flactastic.trackID"
	sidecarRel = ".flactastic/track-ids.json"
	settle     = 2 * time.Second // a file younger than this may still be being written
)

// Network filesystems get polled; inotify never sees changes made by other
// machines.
var networkFS = map[int64]bool{0x6969: true, 0xFF534D42: true, 0xFE534D42: true, 0x517B: true,
	0x65735546: true, 0x00C36400: true, 0x01021997: true}

func (l *Library) scanLoop(ctx context.Context) {
	watch := -1
	defer func() {
		if watch >= 0 {
			unix.Close(watch)
		}
	}()
	for {
		interval := time.Hour
		if l.attach() {
			var st unix.Statfs_t
			network := unix.Statfs(l.User.Root, &st) == nil && networkFS[int64(st.Type)]
			if network {
				interval = 10 * time.Minute
			} else if watch < 0 {
				watch = l.watch(ctx)
			}
			if !network && watch < 0 {
				interval = 10 * time.Minute
			}
			l.scan(watch)
		} else {
			interval = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
			// Let a burst of changes finish before walking.
			for quiet := false; !quiet; {
				select {
				case <-l.wake:
				case <-time.After(settle):
					quiet = true
				case <-ctx.Done():
					return
				}
			}
		case <-time.After(interval):
		}
	}
}

// watch starts an inotify instance whose events wake the scanner. Watches
// are added per directory during each scan.
func (l *Library) watch(ctx context.Context) int {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return -1
	}
	f := os.NewFile(uintptr(fd), "inotify")
	go func() {
		<-ctx.Done()
		f.Close()
	}()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			l.Scan()
		}
	}()
	return fd
}

type indexed struct {
	id                        string
	size, mtime, inode, ctime int64
}

// ScanReport is what a scan asked for from the CLI found.
type ScanReport struct {
	Error    string   `json:"error,omitempty"` // the library could not be opened
	Tracks   int      `json:"tracks"`
	Indexed  int      `json:"indexed"`
	Removed  int      `json:"removed"`
	Failed   int      `json:"failed"`
	Problems []string `json:"problems"` // the first 200 of Failed
}

func (r *ScanReport) problem(rel string, err error) {
	if r.Failed++; len(r.Problems) < 200 {
		r.Problems = append(r.Problems, rel+": "+err.Error())
	}
}

func (l *Library) saveReport(r ScanReport) {
	b, _ := json.Marshal(r)
	l.db.Exec("INSERT OR REPLACE INTO docs VALUES('scan', ?)", b)
}

func (l *Library) scan(watch int) {
	r := l.root.Load()
	var rep ScanReport
	wanted := l.report.Swap(false)
	l.setState(StateScanning)
	known := map[string]indexed{}
	rows, err := l.db.Query("SELECT path, id, size, mtime, inode, ctime FROM tracks WHERE path IS NOT NULL")
	if err == nil {
		for rows.Next() {
			var p string
			var t indexed
			rows.Scan(&p, &t.id, &t.size, &t.mtime, &t.inode, &t.ctime)
			known[p] = t
		}
		rows.Close()
	}
	l.loadSidecar(r)

	seen := map[string]bool{}
	var unreadable, dirs []string
	pending := false
	filepath.WalkDir(r.Path, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(r.Path, p)
		if err != nil {
			unreadable = append(unreadable, rel)
			rep.problem(rel, err)
			return fs.SkipDir
		}
		if rel != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if rel != "." {
				dirs = append(dirs, rel)
			}
			if watch >= 0 {
				unix.InotifyAddWatch(watch, p, unix.IN_CLOSE_WRITE|unix.IN_CREATE|unix.IN_DELETE|unix.IN_MOVED_FROM|unix.IN_MOVED_TO|unix.IN_ATTRIB)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if _, ok := meta.ClassifyExt(strings.TrimPrefix(path.Ext(rel), ".")); !ok {
			return nil
		}
		seen[rel] = true
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st := info.Sys().(*syscall.Stat_t)
		if t, ok := known[rel]; ok && t.size == info.Size() && t.mtime == info.ModTime().UnixNano() && t.inode == int64(st.Ino) &&
			t.ctime == st.Ctim.Nano() {
			return nil
		}
		if time.Since(info.ModTime()) < settle {
			pending = true
			return nil
		}
		l.mu.Lock()
		if err := l.index(r, rel); err != nil {
			slog.Info("skipped unreadable file", "user", l.User.Name, "path", rel, "err", err)
			rep.problem(rel, err)
		} else {
			rep.Indexed++
		}
		l.mu.Unlock()
		return nil
	})

	l.mu.Lock()
	for rel, t := range known {
		if seen[rel] || underAny(rel, unreadable) {
			continue
		}
		// The walk ran unlocked: the server may have moved this track
		// meanwhile, or put a file back at this path.
		var now string
		l.db.QueryRow("SELECT path FROM tracks WHERE id = ?", t.id).Scan(&now)
		if now != rel {
			continue
		}
		if f, err := r.Open(rel, unix.O_RDONLY, 0); err == nil { // a real file, not a symlink
			f.Close()
			continue
		}
		// Forget only on a second look: a move still settling, or a network
		// mount's passing glitch, must not delete the track.
		if !l.missing[rel] {
			l.missing[rel] = true
			pending = true
			continue
		}
		delete(l.missing, rel)
		l.forget(t.id, rel)
		rep.Removed++
	}
	for rel := range l.missing {
		if seen[rel] || known[rel].id == "" {
			delete(l.missing, rel)
		}
	}
	if wanted {
		l.db.QueryRow("SELECT COUNT(*) FROM tracks WHERE path IS NOT NULL").Scan(&rep.Tracks)
		l.saveReport(rep)
	}
	// A folder with audio beneath it can't be pruned; only check the rest.
	full := map[string]bool{}
	for rel := range seen {
		for d := path.Dir(rel); d != "." && !full[d]; d = path.Dir(d) {
			full[d] = true
		}
	}
	var empty []string
	for _, d := range dirs {
		if !full[d] {
			empty = append(empty, d)
		}
	}
	l.prune(r, empty, true)
	l.saveSidecar(r)
	l.sweep(r)
	l.mu.Unlock()
	if pending {
		time.AfterFunc(settle, l.Scan)
	}
	l.setState(StateReady)
	l.wakeHasher()
	// A walk of a large library leaves tens of MB of garbage behind; hand it
	// back now rather than idle at the high-water mark.
	debug.FreeOSMemory()
}

func underAny(rel string, dirs []string) bool {
	for _, d := range dirs {
		if d == "." || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// index reads one file and records it, with its content and audio hashes
// when the caller has just computed them. Callers hold l.mu.
func (l *Library) index(r *Root, rel string, hashes ...string) error {
	f, err := r.Open(rel, unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	// Before the stat: setting the ID xattr changes ctime.
	id := l.identify(f, rel)
	var st unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BASIC_STATS|unix.STATX_BTIME, &st); err != nil {
		return err
	}
	mtime := time.Unix(st.Mtime.Sec, int64(st.Mtime.Nsec))
	added := mtime
	if st.Mask&unix.STATX_BTIME != 0 {
		added = time.Unix(st.Btime.Sec, int64(st.Btime.Nsec))
	}
	ext := strings.TrimPrefix(path.Ext(rel), ".")
	dup, err := unix.Dup(int(f.Fd()))
	if err != nil {
		return err
	}
	tf, err := taglib.Open(dup, ext, false)
	if err != nil {
		return err
	}
	tags := tf.Read()
	pic := tf.Picture()
	tf.Close()

	ctime := time.Unix(st.Ctime.Sec, int64(st.Ctime.Nsec)).UnixNano()
	var oldHash, oldAudio *string
	var oldSize, oldMtime, oldInode, oldCtime int64
	var oldAdded string
	l.db.QueryRow("SELECT hash, audio, size, mtime, inode, ctime, json_extract(data, '$.dateAdded') FROM tracks WHERE id = ?", id).
		Scan(&oldHash, &oldAudio, &oldSize, &oldMtime, &oldInode, &oldCtime, &oldAdded)
	if oldSize != int64(st.Size) || oldMtime != mtime.UnixNano() || oldInode != int64(st.Ino) || oldCtime != ctime {
		oldHash, oldAudio = nil, nil
	}
	if len(hashes) > 1 {
		oldHash, oldAudio = &hashes[0], &hashes[1]
	}
	if a, err := time.Parse(time.RFC3339Nano, oldAdded); err == nil {
		added = a
	}
	t := buildTrack(id, rel, int64(st.Size), tags, pic, added, mtime)
	t.ContentHash, t.AudioHash = oldHash, published(oldAudio)
	data, _ := json.Marshal(t)
	return l.write(func(tx *sql.Tx, seq int64) error {
		// A different track that used to live at this path is gone.
		var other string
		if tx.QueryRow("SELECT id FROM tracks WHERE path = ? AND id != ?", rel, id).Scan(&other) == nil {
			if err := tombstone(tx, other, seq); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO tracks(id, path, seq, size, mtime, inode, ctime, hash, audio, art, search, data) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET path=excluded.path, seq=excluded.seq, size=excluded.size, mtime=excluded.mtime,
			inode=excluded.inode, ctime=excluded.ctime, hash=excluded.hash, audio=excluded.audio, art=excluded.art, search=excluded.search,
			data=excluded.data`,
			id, rel, seq, st.Size, mtime.UnixNano(), st.Ino, ctime, oldHash, oldAudio, t.ArtworkID, searchText(t), data)
		return err
	}, KindTracks)
}

func buildTrack(id, rel string, size int64, tags taglib.Tags, pic []byte, added, modified time.Time) Track {
	ext := strings.TrimPrefix(path.Ext(rel), ".")
	format, _ := meta.ClassifyExt(ext)
	if (format == meta.ALAC || format == meta.AAC) && tags.Codec != "" {
		format = meta.Format(tags.Codec)
	}
	title := tags.Title
	if title == "" {
		title = strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	}
	uid, _ := meta.ParseUUID(id)
	t := Track{
		ID: uid, RelativePath: rel, Format: format, FileSize: size, Title: title,
		Artist: tags.Artist, ArtistIsList: tags.ArtistIsList, AlbumArtist: tags.AlbumArtist,
		AlbumArtistIsList: tags.AlbumArtistIsList, Album: tags.Album, TrackNumber: tags.TrackNumber,
		DiscNumber: tags.DiscNumber, DiscTotal: tags.DiscTotal, Year: tags.Year, Genre: tags.Genre,
		SecondaryGenres: append([]string{}, tags.SecondaryGenres...), IsCompilation: tags.Compilation,
		IsMixCompilation: tags.MixCompilation, Label: tags.Label, Duration: float64(tags.DurationMs) / 1000,
		SampleRate: tags.SampleRate, Channels: tags.Channels, Bitrate: tags.Bitrate, Frames: tags.Frames,
		HasCueSheet: tags.CueSheet != "", DateAdded: meta.Time{Time: added}, Modified: meta.Time{Time: modified},
	}
	if tags.Bits > 0 && format != meta.MP3 && format != meta.AAC {
		t.BitDepth = &tags.Bits
	}
	if pic != nil {
		sum := sha256.Sum256(pic)
		art := hex.EncodeToString(sum[:16])
		t.ArtworkID = &art
	}
	t.TagFingerprint = fingerprint(t)
	return t
}

func fingerprint(t Track) string {
	s := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return meta.Fingerprint(meta.FingerprintInput{
		Title: t.Title, Artist: s(t.Artist), AlbumArtist: s(t.AlbumArtist), Album: s(t.Album),
		TrackNumber: t.TrackNumber, Genre: s(t.Genre), SecondaryGenres: t.SecondaryGenres, Year: t.Year,
		IsCompilation: t.IsCompilation, IsMixCompilation: t.IsMixCompilation,
	})
}

func tombstone(tx *sql.Tx, id string, seq int64) error {
	_, err := tx.Exec("UPDATE tracks SET path = NULL, seq = ?, hash = NULL, audio = NULL WHERE id = ?", seq, id)
	return err
}

// forget records that the track at rel is gone. Callers hold l.mu.
func (l *Library) forget(id, rel string) {
	l.write(func(tx *sql.Tx, seq int64) error { return tombstone(tx, id, seq) }, KindTracks)
	if _, ok := l.sidecar[rel]; ok {
		delete(l.sidecar, rel)
		l.sidecarDirty = true
	}
}

// identify finds a file's trackID as the Mac's TrackIDStore does: the xattr,
// then the sidecar, then a fresh UUID; and writes back whatever was missing.
// A trackID already held by another file that still exists means this file
// is a copy, which gets an ID of its own. Callers hold l.mu.
func (l *Library) identify(f *os.File, rel string) string {
	buf := make([]byte, 64)
	id := ""
	if n, err := unix.Fgetxattr(int(f.Fd()), xattrName, buf); err == nil {
		if u, err := meta.ParseUUID(string(buf[:n])); err == nil {
			id = u.String()
		}
	}
	if id == "" {
		id = l.sidecar[rel]
	}
	if id != "" {
		var other string
		if l.db.QueryRow("SELECT path FROM tracks WHERE id = ? AND path IS NOT NULL", id).Scan(&other) == nil &&
			other != rel && l.root.Load().Exists(other) {
			id = ""
		}
	}
	if id == "" {
		id = meta.NewUUID().String()
	}
	l.setID(f, rel, id)
	return id
}

func (l *Library) setID(f *os.File, rel, id string) {
	buf := make([]byte, 64)
	n, err := unix.Fgetxattr(int(f.Fd()), xattrName, buf)
	if err != nil || string(buf[:n]) != id { // a copy given its own ID keeps the original's until rewritten
		unix.Fsetxattr(int(f.Fd()), xattrName, []byte(id), 0)
	}
	if l.sidecar[rel] != id {
		l.sidecar[rel] = id
		l.sidecarDirty = true
	}
}

func (l *Library) loadSidecar(r *Root) {
	if l.sidecar != nil {
		return
	}
	l.sidecar = map[string]string{}
	if f, err := r.Open(sidecarRel, unix.O_RDONLY, 0); err == nil {
		json.NewDecoder(io.LimitReader(f, 256<<20)).Decode(&l.sidecar)
		f.Close()
	}
}

// saveSidecar keeps the Mac's track-ids.json current, so a Mac opening the
// same folder over a share without xattrs still sees every track's identity.
func (l *Library) saveSidecar(r *Root) {
	if !l.sidecarDirty {
		return
	}
	data, _ := json.Marshal(l.sidecar)
	tmp := ".flactastic/track-ids.json." + meta.NewUUID().String()
	f, err := r.Create(tmp)
	if err != nil {
		return
	}
	_, err = f.Write(data)
	f.Close()
	if err == nil && r.Replace(tmp, sidecarRel) == nil {
		l.sidecarDirty = false
	} else {
		r.Remove(tmp, false)
	}
}

func (l *Library) wakeHasher() {
	select {
	case l.hashing <- struct{}{}:
	default:
	}
}

// published is a stored audio hash as the Track shows it: "" (no audio
// spans for this format) is null, like one not yet computed.
func published(audio *string) *string {
	if audio == nil || *audio == "" {
		return nil
	}
	return audio
}

// hashLoop fills in contentHash and audioHash for every track, one file at
// a time: new tracks first, then tracks hashed before audioHash existed.
func (l *Library) hashLoop(ctx context.Context) {
	buf := make([]byte, 1<<20)
	idle := true
	for {
		r, err := l.Root()
		n, failed := 0, false
		if err == nil {
			n, failed = l.hashBatch(ctx, r, buf)
		}
		if ctx.Err() != nil {
			return
		}
		if n == 0 {
			if !idle {
				idle = true
				debug.FreeOSMemory()
			}
			select {
			case <-ctx.Done():
				return
			case <-l.hashing:
			}
			continue
		}
		idle = false
		if failed {
			l.Scan()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}
}

// hashBatch hashes up to 64 tracks that lack a contentHash, or failing that,
// an audioHash, and publishes each result. It reports how many it tried and
// whether any file couldn't be read.
func (l *Library) hashBatch(ctx context.Context, r *Root, buf []byte) (n int, failed bool) {
	type job struct {
		id, rel                   string
		size, mtime, inode, ctime int64
	}
	var jobs []job
	// Each names its partial index: SQLite's planner would otherwise walk the
	// path index over every track on each wake, long after there's nothing left.
	for _, from := range []string{"tracks INDEXED BY tracks_unhashed WHERE hash IS NULL",
		"tracks INDEXED BY tracks_noaudio WHERE audio IS NULL"} {
		rows, err := l.db.Query("SELECT id, path, size, mtime, inode, ctime FROM " + from + " AND path IS NOT NULL LIMIT 64")
		if err != nil {
			continue
		}
		for rows.Next() {
			var j job
			rows.Scan(&j.id, &j.rel, &j.size, &j.mtime, &j.inode, &j.ctime)
			jobs = append(jobs, j)
		}
		rows.Close()
		if len(jobs) > 0 {
			break
		}
	}
	for _, j := range jobs {
		sum, audio, err := hashFile(ctx, r, j.rel, j.size, j.mtime, j.ctime, buf)
		if ctx.Err() != nil {
			return len(jobs), failed
		}
		l.mu.Lock()
		l.write(func(tx *sql.Tx, seq int64) error {
			if err != nil {
				// Unreadable or changed underneath: the next scan reindexes it.
				_, err := tx.Exec("UPDATE tracks SET mtime = 0 WHERE id = ? AND mtime = ?", j.id, j.mtime)
				return err
			}
			_, err := tx.Exec(`UPDATE tracks SET hash = ?, audio = ?, seq = ?,
				data = json_set(data, '$.contentHash', ?, '$.audioHash', ?)
				WHERE id = ? AND size = ? AND mtime = ? AND inode = ? AND ctime = ?`,
				sum, audio, seq, sum, published(&audio), j.id, j.size, j.mtime, j.inode, j.ctime)
			return err
		}, KindTracks)
		l.mu.Unlock()
		failed = failed || err != nil
	}
	return len(jobs), failed
}

// hashFile streams a file once for its SHA-256 and, from the same bytes,
// the SHA-256 of its audio alone (see audioSpans; "" when the format isn't
// one it knows), and checks the file didn't change meanwhile. A negative
// mtime skips the mtime and ctime checks.
func hashFile(ctx context.Context, r *Root, rel string, size, mtime, ctime int64, buf []byte) (content, audio string, err error) {
	f, err := r.Open(rel, unix.O_RDONLY, 0)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	spans, hasAudio := audioSpans(f, size, strings.TrimPrefix(path.Ext(rel), "."))
	// Hash both while the next chunk is read: buf's two halves take turns,
	// so this is no slower than reading the file and hashing it once.
	h, a := sha256.New(), sha256.New()
	halves := [2][]byte{buf[:len(buf)/2], buf[len(buf)/2:]}
	var wg sync.WaitGroup
	defer wg.Wait() // never return while a hash still reads buf
	for off, cur := int64(0), 0; ; cur ^= 1 {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		n, err := f.Read(halves[cur])
		wg.Wait()
		chunk := halves[cur][:n]
		wg.Add(2)
		go func() { defer wg.Done(); h.Write(chunk) }()
		go func(off int64) {
			defer wg.Done()
			for _, s := range spans {
				if lo, hi := max(s.start, off), min(s.end, off+int64(n)); lo < hi {
					a.Write(chunk[lo-off : hi-off])
				}
			}
		}(off)
		off += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}
	}
	wg.Wait()
	info, err := f.Stat()
	if err != nil || info.Size() != size || (mtime >= 0 && (info.ModTime().UnixNano() != mtime ||
		info.Sys().(*syscall.Stat_t).Ctim.Nano() != ctime)) {
		return "", "", os.ErrInvalid
	}
	if hasAudio {
		audio = hex.EncodeToString(a.Sum(nil))
	}
	return hex.EncodeToString(h.Sum(nil)), audio, nil
}
