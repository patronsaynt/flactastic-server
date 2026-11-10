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
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rivo/uniseg"
	"golang.org/x/sys/unix"

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

// place is where the default profile files t, made unique on disk. A path
// held by t itself counts as free.
func (l *Library) place(r *Root, t Track, self string) (string, error) {
	ops := meta.PlanOrganize([]meta.OrganizerTrack{t.organizerTrack()}, l.profile(nil), l.resolver())
	if ops[0].Status == meta.OpUnchanged {
		return t.RelativePath, nil
	}
	dest := meta.UniqueName(fitComponents(ops[0].Destination), func(p string) bool { return p != self && r.Exists(p) })
	return meta.SanitizeAudioRelativePath(dest)
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
	sum, err := hashFile(context.Background(), r, part, u.size, -1, make([]byte, 1<<20))
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
		dest = meta.UniqueName(dest, r.Exists)
	}
	if err := l.tagID(r, part, id); err != nil {
		return nil, err
	}
	if old != "" && old != dest {
		l.trash(r, id, old)
	}
	if err := r.Rename(part, dest, old != dest); err != nil {
		return nil, err
	}
	l.db.Exec("DELETE FROM uploads WHERE id = ?", u.ID)
	uploadLocks.Delete(u.ID)
	if err := l.index(r, dest); err != nil {
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
	l.saveSidecar(r)
	return nil
}

// Edit writes tags to each track on a copy that then atomically replaces
// the original, and refiles the track through the default profile.
func (l *Library) Edit(ids []meta.UUID, e taglib.Edit, separator string) ([]Track, error) {
	r, err := l.Root()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Track, 0, len(ids))
	for _, id := range ids {
		t, err := l.Track(id)
		if err != nil {
			return out, ErrNotFound
		}
		if err := l.editFile(r, t, e, separator); err != nil {
			return out, err
		}
		l.index(r, t.RelativePath)
		nt, _ := l.Track(id)
		dest, err := l.place(r, nt, nt.RelativePath)
		if err == nil && dest != nt.RelativePath && r.Rename(nt.RelativePath, dest, true) == nil {
			l.moved(id.String(), nt.RelativePath, dest)
			nt, _ = l.Track(id)
		}
		out = append(out, nt)
	}
	l.saveSidecar(r)
	return out, nil
}

func (l *Library) editFile(r *Root, t Track, e taglib.Edit, separator string) error {
	src, err := r.Open(t.RelativePath, unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := incomingDir + meta.NewUUID().String() + ".edit"
	dst, err := r.Create(tmp)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		dst.Close()
		if !ok {
			r.Remove(tmp, false)
		}
	}()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if info, err := src.Stat(); err == nil {
		dst.Chmod(info.Mode().Perm())
	}
	unix.Fsetxattr(int(dst.Fd()), xattrName, []byte(t.ID.String()), 0)
	dup, err := unix.Dup(int(dst.Fd()))
	if err != nil {
		return err
	}
	tf, err := taglib.Open(dup, strings.TrimPrefix(path.Ext(t.RelativePath), "."), true)
	if err != nil {
		return err
	}
	tf.Apply(e, tf.Read(), t.Format == meta.FLAC, separator)
	err = tf.Save()
	tf.Close()
	if err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	if err := r.Rename(tmp, t.RelativePath, false); err != nil {
		return err
	}
	ok = true
	return nil
}

// moved records a rename the server made. Callers hold l.mu.
func (l *Library) moved(id, from, to string) {
	l.write(func(tx *sql.Tx, seq int64) error {
		_, err := tx.Exec("UPDATE tracks SET path = ?, seq = ?, data = json_set(data, '$.relativePath', ?) WHERE id = ?",
			to, seq, to, id)
		return err
	}, KindTracks)
	delete(l.sidecar, from)
	l.sidecar[to] = id
	l.sidecarDirty = true
}

// Organizer plans (protocol §12)

type PlanOp struct {
	TrackID meta.UUID `json:"trackID"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Status  string    `json:"status"`
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
	ops := make([]PlanOp, len(planned))
	lines := make([]string, len(planned))
	for i, o := range planned {
		ops[i] = PlanOp{TrackID: o.ID, From: o.Source, To: fitComponents(o.Destination), Status: string(o.Status)}
		lines[i] = o.ID.String() + ":" + ops[i].To
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), ops, nil
}

// OrganizeApply runs a plan the user approved, if the library still plans
// the same way.
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
	emptied := map[string]bool{}
	for _, o := range ops {
		if o.Status == string(meta.OpUnchanged) {
			unchanged++
			continue
		}
		dest := meta.UniqueName(o.To, r.Exists)
		if _, err := meta.SanitizeAudioRelativePath(dest); err != nil || r.Rename(o.From, dest, true) != nil {
			continue
		}
		l.moved(o.TrackID.String(), o.From, dest)
		emptied[path.Dir(o.From)] = true
		moved++
	}
	if l.profile(profileID).DeleteEmptyOriginals {
		for dir := range emptied {
			for ; dir != "." && dir != "" && !strings.HasPrefix(dir, "."); dir = path.Dir(dir) {
				if r.Remove(dir, true) != nil {
					break
				}
			}
		}
	}
	l.saveSidecar(r)
	return moved, unchanged, nil
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
