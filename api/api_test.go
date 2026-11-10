package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flactastic/server/library"
	"flactastic/server/meta"
	"flactastic/server/store"
)

type env struct {
	t      *testing.T
	url    string
	alice  string // tokens
	bob    string
	aRoot  string
	bRoot  string
	server *Server
}

func copyFile(t *testing.T, src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup runs a real server over two users' libraries made from the fixtures.
func setup(t *testing.T) *env {
	dir := t.TempDir()
	e := &env{t: t, aRoot: filepath.Join(dir, "alice"), bRoot: filepath.Join(dir, "bob")}
	os.MkdirAll(e.aRoot, 0o755)
	os.MkdirAll(e.bRoot, 0o755)
	files, _ := filepath.Glob("../testdata/audio/*")
	for _, f := range files {
		root := e.aRoot
		if strings.HasSuffix(f, ".mp3") || strings.HasSuffix(f, ".wav") {
			root = e.bRoot
		}
		copyFile(t, f, filepath.Join(root, filepath.Base(f)))
	}
	s, err := store.New(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ name, root string }{{"alice", e.aRoot}, {"bob", e.bRoot}} {
		id, err := library.EnsureIdentity(u.root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddUser(u.name, u.name+"-password", u.root, id, u.name == "alice", nil); err != nil {
			t.Fatal(err)
		}
	}
	libs := library.NewManager(s)
	ctx, cancel := context.WithCancel(context.Background())
	go libs.Run(ctx)
	e.server = New(s, libs, "test", "test", "")
	ts := httptest.NewServer(e.server.Handler())
	t.Cleanup(func() { ts.Close(); cancel(); time.Sleep(100 * time.Millisecond) })
	e.url = ts.URL
	e.alice = e.login("alice", "alice-password", 200)["token"].(string)
	e.bob = e.login("bob", "bob-password", 200)["token"].(string)
	e.waitReady(e.alice)
	e.waitReady(e.bob)
	return e
}

func (e *env) waitReady(token string) {
	for i := 0; i < 200; i++ {
		lib := e.get(token, "/v2/library", 200)
		if lib["state"] == "ready" && lib["pendingHashes"].(float64) == 0 && lib["trackCount"].(float64) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatal("library never became ready")
}

func (e *env) do(token, method, path string, body io.Reader, headers map[string]string) *http.Response {
	req, _ := http.NewRequest(method, e.url+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *env) json(token, method, path string, in any, status int) map[string]any {
	e.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	res := e.do(token, method, path, body, nil)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != status {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.StatusCode, status, raw)
	}
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	return out
}

func (e *env) get(token, path string, status int) map[string]any {
	return e.json(token, "GET", path, nil, status)
}

func (e *env) login(user, password string, status int) map[string]any {
	return e.json("", "POST", "/v2/auth/login",
		map[string]string{"username": user, "password": password, "deviceName": "Test", "deviceKind": "linux"}, status)
}

func (e *env) tracks(token string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, t := range e.get(token, "/v2/tracks?since=0&limit=5000", 200)["tracks"].([]any) {
		m := t.(map[string]any)
		out[m["relativePath"].(string)] = m
	}
	return out
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func TestAuth(t *testing.T) {
	e := setup(t)
	e.get("", "/v2/server", 200)
	if c := errCode(e.get("", "/v2/library", 401)); c != "unauthorized" {
		t.Fatal(c)
	}
	e.get("not-a-token", "/v2/library", 401)
	if c := errCode(e.login("alice", "wrong-password", 401)); c != "invalidCredentials" {
		t.Fatal(c)
	}
	if c := errCode(e.login("nobody", "wrong-password", 401)); c != "invalidCredentials" {
		t.Fatal(c)
	}
	// The bad token above counts against this IP too: 5 failures are free.
	for i := 0; i < 2; i++ {
		e.login("alice", "wrong-password", 401)
	}
	e.login("alice", "wrong-password", 429)
	e.server.failures.clear("ip:127.0.0.1")

	// Changing a password revokes the user's other devices.
	other := e.bob
	second := e.login("bob", "bob-password", 200)["token"].(string)
	e.json(second, "PUT", "/v2/me/password", map[string]string{"current": "bob-password", "new": "new-bob-password"}, 204)
	e.get(other, "/v2/me", 401)
	e.get(second, "/v2/me", 200)
	e.json(second, "POST", "/v2/auth/logout", nil, 204)
	e.get(second, "/v2/me", 401)
}

// TestScanParity checks that a track read end to end by the server carries
// the fingerprint the Mac computed for the same file.
func TestScanParity(t *testing.T) {
	e := setup(t)
	raw, _ := os.ReadFile("../testdata/vectors.json")
	var v struct{ Files []map[string]any }
	json.Unmarshal(raw, &v)
	tracks := e.tracks(e.alice)
	for k, t := range e.tracks(e.bob) {
		tracks[k] = t
	}
	for _, f := range v.Files {
		got, ok := tracks[f["file"].(string)]
		if !ok {
			t.Errorf("%s not indexed", f["file"])
			continue
		}
		for _, k := range []string{"tagFingerprint", "contentHash", "title", "artist"} {
			if got[k] != f[k] {
				t.Errorf("%s: %s = %v, Mac %v", f["file"], k, got[k], f[k])
			}
		}
	}
}

func TestFileRanges(t *testing.T) {
	e := setup(t)
	tr := e.tracks(e.alice)["hires.flac"]
	want, _ := os.ReadFile(filepath.Join(e.aRoot, "hires.flac"))
	res := e.do(e.alice, "GET", "/v2/tracks/"+tr["id"].(string)+"/file", nil, map[string]string{"Range": "bytes=100-199"})
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 206 || !bytes.Equal(got, want[100:200]) || res.Header.Get("Content-Type") != "audio/flac" {
		t.Fatalf("range: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	etag := res.Header.Get("ETag")
	if etag != `"sha256:`+tr["contentHash"].(string)+`"` {
		t.Fatalf("etag %s", etag)
	}
	res = e.do(e.alice, "GET", "/v2/tracks/"+tr["id"].(string)+"/file", nil, map[string]string{"If-None-Match": etag})
	res.Body.Close()
	if res.StatusCode != 304 {
		t.Fatalf("conditional: %d", res.StatusCode)
	}
	art := tr["artworkID"].(string)
	for _, size := range []string{"300", "original"} {
		res = e.do(e.alice, "GET", "/v2/art/"+art+"?size="+size, nil, nil)
		res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("art %s: %d", size, res.StatusCode)
		}
	}
}

func upload(e *env, token string, data []byte, filename string, trackID *string) (map[string]any, int) {
	sum := sha256.Sum256(data)
	in := map[string]any{"filename": filename, "size": len(data), "contentHash": hex.EncodeToString(sum[:])}
	if trackID != nil {
		in["trackID"] = *trackID
	}
	u := e.json(token, "POST", "/v2/uploads", in, 201)
	id := u["id"].(string)
	half := len(data) / 2
	send := func(offset int, chunk []byte) *http.Response {
		return e.do(token, "PATCH", "/v2/uploads/"+id, bytes.NewReader(chunk),
			map[string]string{"Upload-Offset": fmt.Sprint(offset), "Content-Type": "application/offset+octet-stream"})
	}
	res := send(0, data[:half])
	res.Body.Close()
	if res.StatusCode != 204 || res.Header.Get("Upload-Offset") != fmt.Sprint(half) {
		e.t.Fatalf("first chunk: %d", res.StatusCode)
	}
	res = send(0, data[half:])
	res.Body.Close()
	if res.StatusCode != 409 {
		e.t.Fatalf("stale offset: %d", res.StatusCode)
	}
	res = send(half, data[half:])
	defer res.Body.Close()
	out := map[string]any{}
	json.NewDecoder(res.Body).Decode(&out)
	return out, res.StatusCode
}

func TestUploadEditDelete(t *testing.T) {
	e := setup(t)
	data, _ := os.ReadFile("../testdata/audio/cur-single.flac")
	out, status := upload(e, e.alice, data, "../../../etc/whatever.FLAC", nil)
	if status != 200 {
		t.Fatalf("upload: %d %v", status, out)
	}
	tr := out["track"].(map[string]any)
	// The default profile: {albumArtist}/{album} ({year})/{track} - {title}.
	if p := tr["relativePath"].(string); !strings.HasPrefix(p, "Daft Punk/") || !strings.HasSuffix(p, ".FLAC") {
		t.Fatalf("placed at %s", p)
	}
	if _, err := os.Stat(filepath.Join(e.aRoot, tr["relativePath"].(string))); err != nil {
		t.Fatal(err)
	}

	bad := append([]byte{}, data...)
	sum := sha256.Sum256([]byte("something else"))
	u := e.json(e.alice, "POST", "/v2/uploads", map[string]any{"filename": "x.flac", "size": len(bad),
		"contentHash": hex.EncodeToString(sum[:])}, 201)
	res := e.do(e.alice, "PATCH", "/v2/uploads/"+u["id"].(string), bytes.NewReader(bad),
		map[string]string{"Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"})
	res.Body.Close()
	if res.StatusCode != 422 {
		t.Fatalf("bad hash: %d", res.StatusCode)
	}
	e.json(e.alice, "POST", "/v2/uploads", map[string]any{"filename": "x.exe", "size": 1, "contentHash": hex.EncodeToString(sum[:])}, 400)

	id := tr["id"].(string)
	edited := e.json(e.alice, "PATCH", "/v2/tracks/"+id, map[string]any{"title": "Renamed", "artist": "A ; B", "year": nil, "isCompilation": true}, 200)["track"].(map[string]any)
	if edited["title"] != "Renamed" || edited["artist"] == tr["artist"] || edited["tagFingerprint"] == tr["tagFingerprint"] ||
		edited["dateAdded"] != tr["dateAdded"] || edited["year"] != nil || edited["id"] != id || edited["isCompilation"] != true {
		t.Fatalf("edit: %v", edited)
	}
	if !strings.Contains(edited["relativePath"].(string), "Renamed") {
		t.Fatalf("not refiled: %s", edited["relativePath"])
	}
	detail := e.get(e.alice, "/v2/tracks/"+id, 200)
	if detail["artistIsList"] != true {
		t.Fatalf("list not written: %v", detail)
	}

	since := e.get(e.alice, "/v2/library", 200)["seq"].(float64)
	e.json(e.alice, "DELETE", "/v2/tracks/"+id, nil, 204)
	feed := e.get(e.alice, fmt.Sprintf("/v2/tracks?since=%d", int(since)), 200)
	if d := feed["deleted"].([]any); len(d) != 1 || d[0] != id {
		t.Fatalf("deleted: %v", feed)
	}
	trash, _ := os.ReadDir(filepath.Join(e.aRoot, ".flactastic", "trash"))
	if len(trash) != 1 {
		t.Fatal("not in trash")
	}
}

func TestOrganizer(t *testing.T) {
	e := setup(t)
	plan := e.json(e.alice, "POST", "/v2/organizer/plan", map[string]any{}, 200)
	e.json(e.alice, "POST", "/v2/organizer/apply", map[string]any{"planHash": "stale"}, 409)
	res := e.json(e.alice, "POST", "/v2/organizer/apply", map[string]any{"planHash": plan["planHash"]}, 200)
	if res["moved"].(float64) == 0 {
		t.Fatal("nothing moved")
	}
	for p := range e.tracks(e.alice) {
		if !strings.Contains(p, "/") {
			t.Fatalf("%s left at the root", p)
		}
		if _, err := os.Stat(filepath.Join(e.aRoot, p)); err != nil {
			t.Fatal(err)
		}
	}
	again := e.json(e.alice, "POST", "/v2/organizer/plan", map[string]any{}, 200)
	for _, op := range again["operations"].([]any) {
		if op.(map[string]any)["status"] == "move" {
			t.Fatalf("still to move: %v", op)
		}
	}
}

func TestUserData(t *testing.T) {
	e := setup(t)
	seq := e.get(e.alice, "/v2/changes?since=0", 200)["seq"].(float64)
	woke := make(chan map[string]any)
	go func() { woke <- e.get(e.alice, fmt.Sprintf("/v2/changes?since=%d&wait=30", int(seq)), 200) }()
	time.Sleep(100 * time.Millisecond)
	id := meta.NewUUID().String()
	pl := map[string]any{"id": id, "name": "Mix", "dateCreated": "2026-01-02T03:04:05.678Z",
		"entries": []any{map[string]any{"id": meta.NewUUID().String(), "relativePath": "x.flac"}}}
	e.json(e.alice, "PUT", "/v2/playlists/"+id, pl, 204)
	select {
	case c := <-woke:
		if fmt.Sprint(c["changed"]) != "[playlists]" {
			t.Fatalf("changed %v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long-poll didn't wake")
	}
	if got := e.get(e.alice, "/v2/playlists/"+id, 200); got["dateCreated"] != "2026-01-02T03:04:05.678Z" {
		t.Fatalf("playlist %v", got)
	}

	p := e.json(e.alice, "PATCH", "/v2/prefs", map[string]any{"streamQuality": "auto", "clients": map[string]any{"mac": map[string]any{"x": 1}}}, 200)
	if p["streamQuality"] != "auto" || p["artistSeparator"] != ", " {
		t.Fatalf("prefs %v", p)
	}
	e.json(e.alice, "PATCH", "/v2/prefs", map[string]any{"streamQuality": "loud"}, 400)

	event := map[string]any{"id": meta.NewUUID().String(), "date": time.Now().UTC().Format(time.RFC3339Nano),
		"trackID": meta.NewUUID().String(), "title": "T", "artist": "Daft Punk", "albumID": "Daft Punk|Discovery",
		"album": "Discovery", "genre": "House", "secondsListened": 300.0, "counted": true}
	body := map[string]any{"events": []any{event}, "contexts": []any{}}
	if n := e.json(e.alice, "POST", "/v2/history", body, 200)["accepted"]; n != 1.0 {
		t.Fatal(n)
	}
	if n := e.json(e.alice, "POST", "/v2/history", body, 200)["accepted"]; n != 0.0 {
		t.Fatal("history not idempotent")
	}
	st := e.get(e.alice, "/v2/stats?tz=Europe/London", 200)
	if st["tracksPlayed"] != 1.0 || st["streakDays"] != 1.0 || st["topGenre"].(map[string]any)["name"] != "House" {
		t.Fatalf("stats %v", st)
	}
	if h := e.get(e.alice, "/v2/history?since=0", 200); len(h["events"].([]any)) != 1 {
		t.Fatalf("history %v", h)
	}

	m := e.json(e.alice, "POST", "/v2/manifest", map[string]any{"filter": map[string]any{"excludedFormats": []string{"alac", "aac"}}}, 200)
	var manifest meta.LibraryManifest
	b, _ := json.Marshal(m)
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, tr := range manifest.Tracks {
		if tr.Format == meta.ALAC || tr.Format == meta.AAC {
			t.Fatalf("filter ignored: %v", tr.Format)
		}
	}
	if len(manifest.Tracks) == 0 || len(manifest.Playlists) != 1 {
		t.Fatalf("manifest: %d tracks, %d playlists", len(manifest.Tracks), len(manifest.Playlists))
	}
}

// TestIsolation proves nothing one user can name reaches another's data.
func TestIsolation(t *testing.T) {
	e := setup(t)
	aliceTrack := e.tracks(e.alice)["hires.flac"]
	id := aliceTrack["id"].(string)
	for _, path := range []string{"/v2/tracks/" + id, "/v2/tracks/" + id + "/file", "/v2/tracks/" + id + "/stream",
		"/v2/art/" + aliceTrack["artworkID"].(string) + "?size=300"} {
		if c := errCode(e.get(e.bob, path, 404)); c != "notFound" {
			t.Fatalf("%s: %s", path, c)
		}
	}
	e.json(e.bob, "PATCH", "/v2/tracks/"+id, map[string]any{"title": "pwned"}, 404)
	e.json(e.bob, "DELETE", "/v2/tracks/"+id, nil, 404)
	if _, ok := e.tracks(e.bob)["hires.flac"]; ok {
		t.Fatal("bob sees alice's track")
	}

	pid := meta.NewUUID().String()
	e.json(e.alice, "PUT", "/v2/playlists/"+pid, map[string]any{"id": pid, "name": "Private", "dateCreated": "2026-01-02T03:04:05.000Z", "entries": []any{}}, 204)
	e.get(e.bob, "/v2/playlists/"+pid, 404)
	e.json(e.bob, "DELETE", "/v2/playlists/"+pid, nil, 404)
	if n := len(e.get(e.bob, "/v2/playlists", 200)["playlists"].([]any)); n != 0 {
		t.Fatal("bob sees alice's playlists")
	}

	sum := sha256.Sum256([]byte("x"))
	u := e.json(e.alice, "POST", "/v2/uploads", map[string]any{"filename": "x.flac", "size": 1, "contentHash": hex.EncodeToString(sum[:])}, 201)
	res := e.do(e.bob, "PATCH", "/v2/uploads/"+u["id"].(string), strings.NewReader("x"),
		map[string]string{"Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"})
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("bob wrote to alice's upload: %d", res.StatusCode)
	}

	// An upload claiming alice's trackID becomes a new track of bob's.
	data, _ := os.ReadFile("../testdata/audio/tagged.mp3")
	out, status := upload(e, e.bob, data, "x.mp3", &id)
	if status != 200 || out["track"].(map[string]any)["id"] != id {
		t.Fatalf("upload: %d %v", status, out)
	}
	if _, err := os.Stat(filepath.Join(e.aRoot, "hires.flac")); err != nil {
		t.Fatal("alice's file was touched")
	}

	aliceDevice := e.get(e.alice, "/v2/me", 200)["device"].(map[string]any)["id"].(string)
	e.json(e.bob, "DELETE", "/v2/devices/"+aliceDevice, nil, 404)
	e.get(e.alice, "/v2/me", 200)
	e.get(e.bob, "/v2/admin/status", 403)
	e.get(e.alice, "/v2/admin/status", 200)
}

// TestSymlinkEscape replaces indexed files with symlinks pointing out of the
// library; the server must refuse to follow them.
func TestSymlinkEscape(t *testing.T) {
	e := setup(t)
	tr := e.tracks(e.alice)["hires.flac"]
	file := filepath.Join(e.aRoot, "hires.flac")
	os.Remove(file)
	os.Symlink(filepath.Join(e.bRoot, "tagged.mp3"), file)
	res := e.do(e.alice, "GET", "/v2/tracks/"+tr["id"].(string)+"/file", nil, nil)
	res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatal("served a file through a symlink out of the library")
	}
	os.Symlink(e.bRoot, filepath.Join(e.aRoot, "linked"))
	e.server.Libs.Get(e.server.libID("alice")).Scan()
	time.Sleep(3 * time.Second)
	for p := range e.tracks(e.alice) {
		if strings.HasPrefix(p, "linked/") || p == "hires.flac" {
			t.Fatalf("indexed %s", p)
		}
	}
}

func (srv *Server) libID(name string) string {
	u, _ := srv.Store.UserByName(name)
	return u.ID
}
