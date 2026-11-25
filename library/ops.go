package library

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"

	"flactastic/server/meta"
	"flactastic/server/taglib"
)

const (
	MaxFileBytes = 2 << 30
	FreeHeadroom = 500_000_000
	incomingDir  = ".flactastic/incoming/"
	trashDir     = ".flactastic/trash/"
	uploadMaxAge = 7 * 24 * time.Hour
	trashMaxAge  = 30 * 24 * time.Hour
	organizerKey = "organizer"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrUnsupported = errors.New("unsupported format")
	ErrTooLarge    = errors.New("too large")
	ErrNoSpace     = errors.New("insufficient storage")
	ErrOffset      = errors.New("offset mismatch")
	ErrHash        = errors.New("hash mismatch")
	ErrInvalid     = errors.New("invalid request")
	ErrPlanStale   = errors.New("plan stale")
	uploadLocks    sync.Map
)

// Duplicate refuses an upload of bytes the library already has.
type Duplicate struct{ Track Track }

func (Duplicate) Error() string { return "duplicate" }

// Organizer is the user's stored profiles (protocol §12).
type Organizer struct {
	Profiles   []meta.Profile `json:"profiles"`
	SelectedID *meta.UUID     `json:"selectedID,omitempty"`
}

func (l *Library) profile(id *meta.UUID) meta.Profile {
	var o Organizer
	l.Doc(organizerKey, &o)
	if id == nil {
		id = o.SelectedID
	}
	for _, p := range o.Profiles {
		if id != nil && p.ID == *id {
			return p
		}
	}
	return meta.DefaultProfile
}

func (t Track) organizerTrack() meta.OrganizerTrack {
	return meta.OrganizerTrack{ID: t.ID, Path: t.RelativePath, Title: t.Title, Artist: t.Artist,
		AlbumArtist: t.AlbumArtist, Album: t.Album, Genre: t.Genre, TrackNumber: t.TrackNumber, Year: t.Year,
		Format: t.Format, ArtistIsList: t.ArtistIsList, AlbumArtistIsList: t.AlbumArtistIsList}
}

func (l *Library) resolver() *meta.ArtistResolver {
	var credits []meta.Credit
	rows, err := l.db.Query(`SELECT json_extract(data, '$.artist'), json_extract(data, '$.albumArtist'),
		json_extract(data, '$.artistIsList'), json_extract(data, '$.albumArtistIsList') FROM tracks WHERE path IS NOT NULL`)
	if err == nil {
		for rows.Next() {
			var c meta.Credit
			rows.Scan(&c.Artist, &c.AlbumArtist, &c.ArtistIsList, &c.AlbumArtistIsList)
			credits = append(credits, c)
		}
		rows.Close()
	}
	return meta.NewArtistResolver(credits, nil)
}

// fitComponents shortens any path component over 255 bytes at a grapheme
// boundary, keeping the file extension. The Mac would fail on such a name.
func fitComponents(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if len(p) <= meta.MaxComponentBytes {
			continue
		}
		ext := ""
		if i == len(parts)-1 {
			ext = path.Ext(p)
			p = strings.TrimSuffix(p, ext)
		}
		out := ""
		for g := uniseg.NewGraphemes(p); g.Next(); {
			if len(out)+len(g.Str())+len(ext) > meta.MaxComponentBytes {
				break
			}
			out += g.Str()
		}
		parts[i] = out + ext
	}
	return strings.Join(parts, "/")
}

// place is where the selected profile files t, made unique. A path held by
// t itself counts as free; one taken on disk, or given to another track by
// the index even if its file is gone, does not.
func (l *Library) place(r *Root, t Track, self string) (string, error) {
	ops := meta.PlanOrganize([]meta.OrganizerTrack{t.organizerTrack()}, l.profile(nil), l.resolver())
	if ops[0].Status == meta.OpUnchanged {
		return t.RelativePath, nil
	}
	dest := meta.UniqueName(fitComponents(ops[0].Destination), l.taken(r, self, t.ID))
	return meta.SanitizeAudioRelativePath(dest)
}

// taken reports a path as unavailable to track id, which already holds self.
func (l *Library) taken(r *Root, self string, id meta.UUID) func(string) bool {
	return func(p string) bool {
		if p == self {
			return false
		}
		var other string
		held := l.db.QueryRow("SELECT id FROM tracks WHERE path = ? AND id != ?", p, id.String()).Scan(&other) == nil
		return held || r.Exists(p)
	}
}

// Uploads

type Upload struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
	ext    string
	size   int64
	hash   string
	track  *string
}

func (l *Library) checkSpace(size int64) error {
	s := l.Stats()
	var staged int64
	l.db.QueryRow("SELECT coalesce(sum(size), 0) FROM uploads").Scan(&staged)
	if q := l.User.Quota; q != nil && s.UsedBytes+staged+size > *q {
		return ErrNoSpace
	}
	if s.FreeBytes < uint64(size)+FreeHeadroom {
		return ErrNoSpace
	}
	return nil
}

func (l *Library) CreateUpload(filename string, size int64, hash string, track *meta.UUID) (Upload, error) {
	ext := strings.TrimPrefix(path.Ext(filename), ".")
	if _, ok := meta.ClassifyExt(ext); !ok {
		return Upload{}, ErrUnsupported
	}
	if size < 0 || size > MaxFileBytes {
		return Upload{}, ErrTooLarge
	}
	hash = strings.ToLower(hash)
	if b, err := hex.DecodeString(hash); err != nil || len(b) != sha256.Size {
		return Upload{}, fmt.Errorf("contentHash is not a SHA-256: %w", ErrInvalid)
	}
	var trackID *string
	if track != nil {
		s := track.String()
		trackID = &s
	}
	var id string
	if track == nil && l.db.QueryRow("SELECT id FROM tracks WHERE hash = ? AND size = ? AND path IS NOT NULL", hash, size).Scan(&id) == nil {
		if have, err := meta.ParseUUID(id); err == nil {
			if t, err := l.Track(have); err == nil {
				return Upload{}, Duplicate{t}
			}
		}
	}
	if l.db.QueryRow("SELECT id FROM uploads WHERE hash = ? AND size = ? AND track IS ?", hash, size, trackID).Scan(&id) == nil {
		return l.UploadStatus(id)
	}
	r, err := l.Root()
	if err != nil {
		return Upload{}, err
	}
	if err := l.checkSpace(size); err != nil {
		return Upload{}, err
	}
	id = meta.NewUUID().String()
	f, err := r.Create(incomingDir + id + ".part")
	if err != nil {
		return Upload{}, err
	}
	f.Close()
	_, err = l.db.Exec("INSERT INTO uploads VALUES(?,?,?,?,?,?)", id, ext, size, hash, trackID, time.Now().Unix())
	return Upload{ID: id}, err
}

func (l *Library) upload(id string) (Upload, error) {
	u := Upload{ID: id}
	err := l.db.QueryRow("SELECT ext, size, hash, track FROM uploads WHERE id = ?", id).Scan(&u.ext, &u.size, &u.hash, &u.track)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return u, err
}

func (l *Library) UploadStatus(id string) (Upload, error) {
	u, err := l.upload(id)
	if err != nil {
		return u, err
	}
	r, err := l.Root()
	if err != nil {
		return u, err
	}
	f, err := r.Open(incomingDir+id+".part", unix.O_RDONLY, 0)
	if err != nil {
		return u, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err == nil {
		u.Offset = info.Size()
	}
	return u, err
}

// AppendUpload writes the next bytes of an upload at offset. When the last
// byte arrives it verifies and installs the file and returns its track.
func (l *Library) AppendUpload(id string, offset int64, body io.Reader) (Upload, *Track, error) {
	lock, _ := uploadLocks.LoadOrStore(id, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	u, err := l.UploadStatus(id)
	if err != nil {
		return u, nil, err
	}
	if offset != u.Offset {
		return u, nil, ErrOffset
	}
	r, _ := l.Root()
	part := incomingDir + id + ".part"
	f, err := r.Open(part, unix.O_WRONLY|unix.O_APPEND, 0)
	if err != nil {
		return u, nil, err
	}
	n, err := io.Copy(f, io.LimitReader(body, u.size-u.Offset))
	u.Offset += n
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return u, nil, err
	}
	if extra, _ := body.Read(make([]byte, 1)); extra > 0 {
		return u, nil, ErrTooLarge
	}
	if u.Offset < u.size {
		return u, nil, nil
	}
	t, err := l.finishUpload(r, u)
	return u, t, err
}

func (l *Library) CancelUpload(id string) error {
	if _, err := l.upload(id); err != nil {
		return err
	}
	if r, err := l.Root(); err == nil {
		r.Remove(incomingDir+id+".part", false)
	}
	uploadLocks.Delete(id)
	_, err := l.db.Exec("DELETE FROM uploads WHERE id = ?", id)
	return err
}

func (l *Library) finishUpload(r *Root, u Upload) (*Track, error) {
	part := incomingDir + u.ID + ".part"
	sum, err := hashFile(context.Background(), r, part, u.size, -1, -1, make([]byte, 1<<20))
	if err != nil || sum != u.hash {
		l.CancelUpload(u.ID)
		return nil, ErrHash
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, err := l.readStaged(r, part, u.ext)
	if err != nil {
		l.CancelUpload(u.ID)
		return nil, ErrUnsupported
	}
	id := meta.NewUUID().String()
	old := ""
	if u.track != nil {
		id = *u.track
		l.db.QueryRow("SELECT path FROM tracks WHERE id = ? AND path IS NOT NULL", id).Scan(&old)
	}
	t.ID, _ = meta.ParseUUID(id)
	t.RelativePath = old
	if old == "" {
		t.RelativePath = "upload." + u.ext
	}
	dest, err := l.place(r, t, old)
	if err != nil {
		return nil, err
	}
	if old == "" && dest == t.RelativePath {
		dest = meta.UniqueName(dest, l.taken(r, "", t.ID))
	}
	if err := l.tagID(r, part, id); err != nil {
		return nil, err
	}
	if old != "" && old != dest {
		l.trash(r, id, old)
	}
	place := func() error { return r.Rename(part, dest, true) }
	if old == dest {
		place = func() error { return r.Replace(part, dest) }
	}
	if err := place(); err != nil {
		return nil, err
	}
	if old != "" && old != dest {
		l.tidy(r, path.Dir(old))
	}
	l.db.Exec("DELETE FROM uploads WHERE id = ?", u.ID)
	uploadLocks.Delete(u.ID)
	if err := l.index(r, dest, u.hash); err != nil {
		return nil, err
	}
	l.saveSidecar(r)
	nt, err := l.Track(t.ID)
	return &nt, err
}

// readStaged reads tags from a file that isn't in the library yet.
func (l *Library) readStaged(r *Root, rel, ext string) (Track, error) {
	f, err := r.Open(rel, unix.O_RDONLY, 0)
	if err != nil {
		return Track{}, err
	}
	defer f.Close()
	dup, err := unix.Dup(int(f.Fd()))
	if err != nil {
		return Track{}, err
	}
	tf, err := taglib.Open(dup, ext, false)
	if err != nil {
		return Track{}, err
	}
	defer tf.Close()
	return buildTrack("", "x."+ext, 0, tf.Read(), nil, time.Time{}, time.Time{}), nil
}

func (l *Library) tagID(r *Root, rel, id string) error {
	f, err := r.Open(rel, unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	l.loadSidecar(r)
	unix.Fsetxattr(int(f.Fd()), xattrName, []byte(id), 0)
	return nil
}

// trash moves a file out of the library into .flactastic/trash.
func (l *Library) trash(r *Root, id, rel string) error {
	dest := fmt.Sprintf("%s%d-%s%s", trashDir, time.Now().Unix(), id, path.Ext(rel))
	if err := r.Rename(rel, dest, true); err != nil {
		return err
	}
	delete(l.sidecar, rel)
	l.sidecarDirty = true
	return nil
}

func (l *Library) Delete(id meta.UUID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, err := l.Track(id)
	if err != nil {
		return ErrNotFound
	}
	r, err := l.Root()
	if err != nil {
		return err
	}
	if err := l.trash(r, t.ID.String(), t.RelativePath); err != nil {
		return err
	}
	l.forget(t.ID.String(), t.RelativePath)
	l.tidy(r, path.Dir(t.RelativePath))
	l.saveSidecar(r)
	return nil
}

// Edit writes tags to each track on a copy that then atomically replaces
// the original, and refiles the track through the selected profile. Every
// track is checked before any is written, so a request that names a missing
// track changes nothing.
func (l *Library) Edit(ids []meta.UUID, e taglib.Edit, separator string) ([]Track, error) {
	r, err := l.Root()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tracks := make([]Track, len(ids))
	for i, id := range ids {
		if tracks[i], err = l.Track(id); err != nil {
			return nil, ErrNotFound
		}
		if err := l.checkFile(r, tracks[i]); err != nil {
			return nil, err
		}
	}
	out := make([]Track, 0, len(ids))
	var left []string
	defer func() { l.tidy(r, left...) }()
	for i, t := range tracks {
		if err := l.editFile(r, t, e, separator); err != nil {
			return out, fmt.Errorf("edited %d of %d tracks: %w", i, len(tracks), err)
		}
		l.index(r, t.RelativePath)
		nt, _ := l.Track(t.ID)
		if dest, err := l.place(r, nt, nt.RelativePath); err == nil && dest != nt.RelativePath {
			if err := r.Rename(nt.RelativePath, dest, true); err != nil {
				slog.Warn("edit: track left in place", "user", l.User.Name, "from", nt.RelativePath, "to", dest, "err", err)
			} else if err := l.moved(t.ID.String(), nt.RelativePath, dest); err != nil {
				// The index can't follow: put the file back rather than lose it.
				slog.Error("edit: refile undone", "user", l.User.Name, "from", nt.RelativePath, "to", dest, "err", err)
				r.Rename(dest, nt.RelativePath, true)
				l.sidecar[nt.RelativePath] = t.ID.String()
				delete(l.sidecar, dest)
			} else {
				left = append(left, path.Dir(nt.RelativePath))
				nt, _ = l.Track(t.ID)
			}
		}
		out = append(out, nt)
	}
	l.saveSidecar(r)
	return out, nil
}

// checkFile makes sure t's own file is at its indexed path. Missing is
// ErrNotFound; a file carrying another track's ID means the index is behind
// the disk, so a scan is started and the edit refused.
func (l *Library) checkFile(r *Root, t Track) error {
	f, err := r.Open(t.RelativePath, unix.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		l.Scan()
		return fmt.Errorf("%w: %s: %v", ErrNotFound, t.RelativePath, err)
	}
	if err != nil {
		return fmt.Errorf("open %s: %v", t.RelativePath, err)
	}
	defer f.Close()
	buf := make([]byte, 64)
	if n, err := unix.Fgetxattr(int(f.Fd()), xattrName, buf); err == nil {
		if other := string(buf[:n]); !strings.EqualFold(other, t.ID.String()) {
			l.Scan()
			return fmt.Errorf("%s holds track %s, not %s: the index is behind the disk; rescanning", t.RelativePath, other, t.ID)
		}
	}
	return nil
}

// editFile rewrites t's tags on a copy beside the original, then renames the
// copy over it. The copy stays in the same folder, which every filesystem
// can rename within atomically. Errors name the failing step and never pass
// on ENOENT, which would read as a missing track.
func (l *Library) editFile(r *Root, t Track, e taglib.Edit, separator string) (err error) {
	step := func(s string, err error) error { return fmt.Errorf("edit %s: %s: %v", t.RelativePath, s, err) }
	src, err := r.Open(t.RelativePath, unix.O_RDONLY, 0)
	if err != nil {
		return step("open", err)
	}
	defer src.Close()
	ext := path.Ext(t.RelativePath)
	tmp := path.Join(path.Dir(t.RelativePath), ".flactastic-edit-"+meta.NewUUID().String()+ext)
	dst, err := r.Create(tmp)
	if err != nil {
		return step("create temporary copy", err)
	}
	closed, ok := false, false
	defer func() {
		if !closed {
			dst.Close()
		}
		if !ok {
			r.Remove(tmp, false)
		}
	}()
	if _, err := io.Copy(dst, src); err != nil {
		return step("copy", err)
	}
	if info, err := src.Stat(); err == nil {
		dst.Chmod(info.Mode().Perm())
	}
	unix.Fsetxattr(int(dst.Fd()), xattrName, []byte(t.ID.String()), 0) // optional: the sidecar also holds it
	dup, err := unix.Dup(int(dst.Fd()))
	if err != nil {
		return step("dup", err)
	}
	tf, err := taglib.Open(dup, strings.TrimPrefix(ext, "."), true)
	if err != nil {
		return step("read tags", err)
	}
	tf.Apply(e, tf.Read(), t.Format == meta.FLAC, separator)
	err = tf.Save()
	tf.Close()
	if err != nil {
		return step("write tags", err)
	}
	if err := dst.Sync(); err != nil {
		return step("sync", err)
	}
	// Some network filesystems won't rename or replace a file that is open.
	closed = true
	if err := dst.Close(); err != nil {
		return step("close", err)
	}
	src.Close()
	if err := r.Replace(tmp, t.RelativePath); err != nil {
		return step("replace original", err)
	}
	ok = true
	return nil
}

// moved records a rename the server made. Callers hold l.mu.
func (l *Library) moved(id, from, to string) error {
	delete(l.sidecar, from)
	l.sidecar[to] = id
	l.sidecarDirty = true
	return l.write(func(tx *sql.Tx, seq int64) error {
		_, err := tx.Exec("UPDATE tracks SET path = ?, seq = ?, data = json_set(data, '$.relativePath', ?) WHERE id = ?",
			to, seq, to, id)
		return err
	}, KindTracks)
}

// Organizer plans (protocol §12)

type PlanOp struct {
	TrackID meta.UUID `json:"trackID"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Status  string    `json:"status"`
	// Renders is the name the profile gives the track before any number is
	// added. Tracks that collide share it exactly, so it groups them.
	Renders string `json:"renders"`
}

func (l *Library) OrganizePlan(profileID *meta.UUID) (string, []PlanOp, error) {
	var tracks []meta.OrganizerTrack
	err := l.Tracks(func(data []byte) error {
		var t Track
		if err := json.Unmarshal(data, &t); err != nil {
			return err
		}
		tracks = append(tracks, t.organizerTrack())
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	planned := meta.PlanOrganize(tracks, l.profile(profileID), l.resolver())

	// Collisions get their numbers here, so apply never picks a name and a
	// settled library plans no moves.
	key := pathKey
	held := map[string]bool{}  // paths the plan's own tracks hold now
	plain := map[string]bool{} // every rendered destination
	groups := map[string][]int{}
	var order []string
	for i, o := range planned {
		held[key(o.Source)] = true
		dest := fitComponents(o.Destination)
		k := key(dest)
		if groups[k] == nil {
			order = append(order, k)
		}
		plain[k] = true
		groups[k] = append(groups[k], i)
	}
	listed := map[string]map[string]bool{}
	outside := func(p string) bool { // taken on disk by a file the plan doesn't hold
		if held[key(p)] {
			return false
		}
		dir := path.Dir(p)
		if listed[key(dir)] == nil {
			listed[key(dir)] = map[string]bool{}
			if r, err := l.Root(); err == nil {
				entries, _ := os.ReadDir(path.Join(r.Path, dir))
				for _, e := range entries {
					listed[key(dir)][key(path.Base(e.Name()))] = true
				}
			}
		}
		return listed[key(dir)][key(path.Base(p))]
	}
	sort.Strings(order)
	to, renders := make([]string, len(planned)), make([]string, len(planned))
	numberedGroup := make([]bool, len(planned))
	assigned := map[string]bool{}
	for _, k := range order {
		members := groups[k]
		dest := fitComponents(planned[members[0]].Destination)
		ext := path.Ext(dest)
		stem := strings.TrimSuffix(dest, ext)
		numbered := func(n int) string {
			s := stem
			for len(path.Base(s))+len(fmt.Sprintf(" (%d)", n))+len(ext) > meta.MaxComponentBytes {
				_, size := utf8.DecodeLastRuneInString(s)
				s = s[:len(s)-size]
			}
			return fmt.Sprintf("%s (%d)%s", s, n, ext)
		}
		// Rank: the track at the plain name, then the lowest existing number,
		// then track ID.
		rank := func(i int) int {
			src := key(planned[i].Source)
			if src == k {
				return 0
			}
			pre, suf := key(stem+" ("), key(")"+ext)
			if strings.HasPrefix(src, pre) && strings.HasSuffix(src, suf) && len(src) > len(pre)+len(suf) {
				if n, err := strconv.Atoi(src[len(pre) : len(src)-len(suf)]); err == nil && n > 0 {
					return n
				}
			}
			return math.MaxInt
		}
		sort.SliceStable(members, func(a, b int) bool {
			ra, rb := rank(members[a]), rank(members[b])
			if ra != rb {
				return ra < rb
			}
			return planned[members[a]].ID.String() < planned[members[b]].ID.String()
		})
		n := 0
		for j, i := range members {
			name := dest
			if j > 0 || outside(dest) {
				for n++; plain[key(numbered(n))] || assigned[key(numbered(n))] || outside(numbered(n)); n++ {
				}
				name = numbered(n)
			}
			assigned[key(name)] = true
			to[i], renders[i] = name, dest
			numberedGroup[i] = len(members) > 1 || name != dest
		}
	}

	ops := make([]PlanOp, len(planned))
	lines := make([]string, len(planned))
	for i, o := range planned {
		status := meta.OpMove
		switch {
		case key(o.Source) == key(to[i]):
			status = meta.OpUnchanged
		case numberedGroup[i]:
			status = meta.OpConflict
		}
		ops[i] = PlanOp{TrackID: o.ID, From: o.Source, To: to[i], Status: string(status), Renders: renders[i]}
		lines[i] = o.ID.String() + ":" + ops[i].To
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), ops, nil
}

// pathKey compares paths the way a case-insensitive, normalization-insensitive
// disk does: case-folded and NFC.
func pathKey(p string) string {
	for i := 0; i < len(p); i++ {
		if p[i] >= 0x80 {
			return meta.NFC(cases.Fold().String(p))
		}
	}
	return strings.ToLower(p)
}

// OrganizeApply runs a plan the user approved, if the library still plans
// the same way. Each file goes to exactly its planned path; a file that
// another move needs out of the way goes through a temporary name first.
func (l *Library) OrganizeApply(profileID *meta.UUID, planHash string) (moved, unchanged int, err error) {
	r, err := l.Root()
	if err != nil {
		return 0, 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	hash, ops, err := l.OrganizePlan(profileID)
	if err != nil {
		return 0, 0, err
	}
	if hash != planHash {
		return 0, 0, ErrPlanStale
	}
	key := pathKey
	targets := map[string]bool{}
	for _, o := range ops {
		if o.Status != string(meta.OpUnchanged) {
			targets[key(o.To)] = true
		}
	}
	var emptied []string
	defer func() {
		if l.profile(profileID).DeleteEmptyOriginals {
			l.prune(r, emptied, false)
		}
		l.saveSidecar(r)
	}()
	from := map[int]string{}
	before := map[int]unix.Stat_t{}
	for i, o := range ops {
		if o.Status == string(meta.OpUnchanged) {
			unchanged++
			continue
		}
		if _, err := meta.SanitizeAudioRelativePath(o.To); err != nil {
			return moved, unchanged, fmt.Errorf("organize %s: %w", o.To, err)
		}
		from[i] = o.From
		before[i], _ = r.Stat(o.From)
		if targets[key(o.From)] {
			// A visible name, so a crash here leaves the file in the library.
			ext := path.Ext(o.From)
			tmp := strings.TrimSuffix(o.From, ext) + " (organizing " + o.TrackID.String()[:8] + ")" + ext
			if err := r.Rename(o.From, tmp, true); err != nil {
				l.restore(r, ops, from)
				return moved, unchanged, fmt.Errorf("organize %s: %w", o.From, err)
			}
			// The index follows, freeing the path for the track moving in.
			from[i] = tmp
			if err := l.moved(o.TrackID.String(), o.From, tmp); err != nil {
				l.restore(r, ops, from)
				return moved, unchanged, fmt.Errorf("organize %s: %w", o.From, err)
			}
		}
	}
	for i, o := range ops {
		src, ok := from[i]
		if !ok {
			continue
		}
		if err := r.Rename(src, o.To, true); err != nil {
			l.restore(r, ops, from)
			return moved, unchanged, fmt.Errorf("organize %s -> %s: %w", o.From, o.To, err)
		}
		delete(from, i)
		if err := l.moved(o.TrackID.String(), src, o.To); err != nil {
			l.restore(r, ops, from)
			return moved, unchanged, fmt.Errorf("organize %s -> %s: index: %w", o.From, o.To, err)
		}
		l.renamedCtime(r, o.TrackID.String(), o.To, before[i])
		emptied = append(emptied, path.Dir(o.From))
		moved++
	}
	return moved, unchanged, nil
}

// restore puts files parked under temporary names back where they were,
// or beside it when another move has taken that path.
func (l *Library) restore(r *Root, ops []PlanOp, from map[int]string) {
	for i, src := range from {
		if src == ops[i].From {
			continue
		}
		dest := ops[i].From
		if r.Rename(src, dest, true) != nil {
			if dest = meta.UniqueName(dest, r.Exists); r.Rename(src, dest, true) != nil {
				continue // still at src, which the index already records
			}
		}
		l.moved(ops[i].TrackID.String(), src, dest)
	}
}

// renamedCtime records the ctime a rename gave a file, if the index was
// current for it beforehand, so the move alone doesn't force a re-hash.
func (l *Library) renamedCtime(r *Root, id, to string, before unix.Stat_t) {
	after, err := r.Stat(to)
	if err != nil || after.Ino != before.Ino || after.Size != before.Size || after.Mtim != before.Mtim {
		return
	}
	l.db.Exec("UPDATE tracks SET ctime = ? WHERE id = ? AND ctime = ? AND size = ? AND mtime = ? AND inode = ?",
		after.Ctim.Nano(), id, before.Ctim.Nano(), before.Size, before.Mtim.Nano(), before.Ino)
}

// sweep deletes staged uploads and trash past their lifetimes.
func (l *Library) sweep(r *Root) {
	cutoff := time.Now().Add(-uploadMaxAge).Unix()
	rows, err := l.db.Query("SELECT id FROM uploads WHERE created < ?", cutoff)
	if err == nil {
		var ids []string
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			l.CancelUpload(id)
		}
	}
	entries, _ := os.ReadDir(path.Join(r.Path, trashDir))
	for _, e := range entries {
		var stamp int64
		if _, err := fmt.Sscanf(e.Name(), "%d-", &stamp); err == nil && time.Since(time.Unix(stamp, 0)) > trashMaxAge {
			r.Remove(trashDir+e.Name(), false)
		}
	}
}
