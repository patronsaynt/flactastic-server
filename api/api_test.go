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
	"path"
	"path/filepath"
	"sort"
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
	// Until the library is open the server answers 503, so any status is fine here.
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		res := e.do(token, "GET", "/v2/library", nil, nil)
		var lib map[string]any
		json.NewDecoder(res.Body).Decode(&lib)
		res.Body.Close()
		if res.StatusCode == 200 && lib["state"] == "ready" && lib["pendingHashes"].(float64) == 0 && lib["trackCount"].(float64) > 0 {
			return
		}
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
	// A backed-off IP still serves valid tokens; only unknown ones are refused.
	e.get(e.bob, "/v2/me", 200)
	e.get("stale-token", "/v2/me", 429)
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
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	// The library already has these bytes, so a second copy is refused and nothing is staged.
	refused := func(path string) {
		t.Helper()
		before := len(e.tracks(e.alice))
		dup := e.json(e.alice, "POST", "/v2/uploads", map[string]any{"filename": "x.flac", "size": len(data), "contentHash": hash}, 409)
		have, _ := dup["error"].(map[string]any)["track"].(map[string]any)
		staged, _ := os.ReadDir(filepath.Join(e.aRoot, ".flactastic", "incoming"))
		if errCode(dup) != "duplicate" || have["relativePath"] != path || len(e.tracks(e.alice)) != before || len(staged) != 0 {
			t.Fatalf("duplicate of %s: %v, %d staged", path, dup, len(staged))
		}
	}
	refused("cur-single.flac")
	e.json(e.alice, "DELETE", "/v2/tracks/"+e.tracks(e.alice)["cur-single.flac"]["id"].(string), nil, 204)
	out, status := upload(e, e.alice, data, "../../../etc/whatever.FLAC", nil)
	if status != 200 {
		t.Fatalf("upload: %d %v", status, out)
	}
	tr := out["track"].(map[string]any)
	if tr["contentHash"] != hash {
		t.Fatalf("contentHash %v, want %s", tr["contentHash"], hash)
	}
	refused(tr["relativePath"].(string))
	// A replacement names its track, so the same bytes are accepted.
	trackID := tr["id"].(string)
	if again, status := upload(e, e.alice, data, "x.flac", &trackID); status != 200 || again["track"].(map[string]any)["id"] != trackID ||
		again["track"].(map[string]any)["contentHash"] != hash {
		t.Fatalf("replacement: %d %v", status, again)
	}
	// The default profile: {albumArtist}/{album} ({year})/{track} - {title}.
	if p := tr["relativePath"].(string); !strings.HasPrefix(p, "Daft Punk/") || !strings.HasSuffix(p, ".FLAC") {
		t.Fatalf("placed at %s", p)
	}
	if _, err := os.Stat(filepath.Join(e.aRoot, tr["relativePath"].(string))); err != nil {
		t.Fatal(err)
	}

	bad := append([]byte{}, data...)
	sum = sha256.Sum256([]byte("something else"))
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
	if len(trash) != 2 { // the original deleted above, and this one
		t.Fatalf("%d files in trash", len(trash))
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
func TestUserItems(t *testing.T) {
	e := setup(t)
	put := func(token, name, body string, status int) string {
		t.Helper()
		res := e.do(token, "PUT", "/v2/userdata/"+name, strings.NewReader(body), nil)
		res.Body.Close()
		if res.StatusCode != status {
			t.Fatalf("PUT %s: %d, want %d", name, res.StatusCode, status)
		}
		return res.Header.Get("ETag")
	}
	etag := put(e.alice, "artists/abc.json", `{"displayName":"A"}`, 204)
	seq := e.get(e.alice, "/v2/changes?since=0", 200)["seq"].(float64)
	// The same bytes again are not a change.
	put(e.alice, "artists/abc.json", `{"displayName":"A"}`, 204)
	if again := e.get(e.alice, "/v2/changes?since=0", 200)["seq"].(float64); again != seq {
		t.Fatal("an identical put advanced the sequence")
	}
	if kinds := fmt.Sprint(e.get(e.alice, "/v2/changes?since=0", 200)["changed"]); !strings.Contains(kinds, "userdata") {
		t.Fatal(kinds)
	}
	items := e.get(e.alice, "/v2/userdata", 200)["items"].([]any)
	if len(items) != 1 || `"`+items[0].(map[string]any)["etag"].(string)+`"` != etag {
		t.Fatal(items)
	}
	res := e.do(e.alice, "GET", "/v2/userdata/artists/abc.json", nil, map[string]string{"If-None-Match": etag})
	res.Body.Close()
	if res.StatusCode != 304 {
		t.Fatal(res.StatusCode)
	}
	res = e.do(e.alice, "GET", "/v2/userdata/artists/abc.json", nil, nil)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != `{"displayName":"A"}` {
		t.Fatal(string(body))
	}
	// One user's data is invisible to another.
	e.get(e.bob, "/v2/userdata/artists/abc.json", 404)
	if n := len(e.get(e.bob, "/v2/userdata", 200)["items"].([]any)); n != 0 {
		t.Fatal(n)
	}
	put(e.alice, ".hidden", "x", 400)
	put(e.alice, "a/.x", "x", 400)
	put(e.alice, "a%20b", "x", 400)
	put(e.alice, "big", strings.Repeat("x", 8<<20+1), 413)
	e.json(e.alice, "DELETE", "/v2/userdata/artists/abc.json", nil, 204)
	e.json(e.alice, "DELETE", "/v2/userdata/artists/abc.json", nil, 404)
}

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
	// The rescan drops the replaced track; wait for that rather than guess its timing.
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(100 * time.Millisecond) {
		if _, still := e.tracks(e.alice)["hires.flac"]; !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still indexing a symlink as hires.flac")
		}
	}
	for p := range e.tracks(e.alice) {
		if strings.HasPrefix(p, "linked/") {
			t.Fatalf("indexed %s", p)
		}
	}
}

func (srv *Server) libID(name string) string {
	u, _ := srv.Store.UserByName(name)
	return u.ID
}

func exists(parts ...string) bool {
	_, err := os.Lstat(filepath.Join(parts...))
	return err == nil
}

// relocate moves one of alice's tracks on disk and waits for a scan to follow it.
func (e *env) relocate(from, to string) {
	os.MkdirAll(filepath.Dir(filepath.Join(e.aRoot, to)), 0o755)
	if err := os.Rename(filepath.Join(e.aRoot, from), filepath.Join(e.aRoot, to)); err != nil {
		e.t.Fatal(err)
	}
	e.server.Libs.Get(e.server.libID("alice")).Scan()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(100 * time.Millisecond) {
		if _, ok := e.tracks(e.alice)[to]; ok {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s never indexed", to)
		}
	}
}

func (e *env) organize() {
	plan := e.json(e.alice, "POST", "/v2/organizer/plan", map[string]any{}, 200)
	e.json(e.alice, "POST", "/v2/organizer/apply", map[string]any{"planHash": plan["planHash"]}, 200)
}

func TestPruneFolders(t *testing.T) {
	e := setup(t)
	a := func(p string) string { return filepath.Join(e.aRoot, p) }
	litter := func(dir string) {
		os.MkdirAll(a(dir), 0o755)
		os.WriteFile(a(dir+"/cover.jpg"), []byte("x"), 0o644)
		os.WriteFile(a(dir+"/.DS_Store"), []byte("x"), 0o644)
	}
	e.organize()
	var paths []string
	for p := range e.tracks(e.alice) {
		paths = append(paths, p)
	}
	home, linked, dotted := paths[0], paths[1], paths[2]
	album := filepath.Dir(home)

	// Organize: leftovers go with the folder, up to the first folder with audio.
	e.relocate(home, album+"/extra/deep/x"+filepath.Ext(home))
	litter(album + "/extra/deep")
	e.relocate(linked, "linkhome/in/y"+filepath.Ext(linked))
	os.Symlink(e.bRoot, a("linkhome/ln"))
	e.relocate(dotted, "dothome/z"+filepath.Ext(dotted))
	os.MkdirAll(a("dothome/.stash"), 0o755)
	e.organize()
	if exists(a(album+"/extra")) || !exists(a(home)) {
		t.Fatal("organize left the emptied folders, or removed the album folder")
	}
	if exists(a("linkhome/in")) || !exists(a("linkhome/ln")) || !exists(e.bRoot, "tagged.mp3") {
		t.Fatal("a folder holding a symlink was removed or followed")
	}
	if !exists(a("dothome/.stash")) || !exists(a(".flactastic")) {
		t.Fatal("a dot-folder was removed")
	}

	// Refile after a tag edit, then delete.
	id := e.tracks(e.alice)[home]["id"].(string)
	e.relocate(home, "solo/one/x"+filepath.Ext(home))
	litter("solo/one")
	moved := e.json(e.alice, "PATCH", "/v2/tracks/"+id, map[string]any{"album": "Elsewhere"}, 200)["track"].(map[string]any)
	now := moved["relativePath"].(string)
	if exists(a("solo")) || !exists(a(now)) {
		t.Fatalf("refile to %s left solo/ behind", now)
	}
	e.relocate(now, "gone/one/x"+filepath.Ext(home))
	litter("gone/one")
	e.json(e.alice, "DELETE", "/v2/tracks/"+id, nil, 204)
	if exists(a("gone")) || !exists(e.aRoot) {
		t.Fatal("delete left gone/ behind, or took the root")
	}

	// With the setting off, only a scan removes folders, and only empty ones.
	off := meta.DefaultProfile
	off.DeleteEmptyOriginals = false
	e.json(e.alice, "PUT", "/v2/organizer", map[string]any{"profiles": []meta.Profile{off}, "selectedID": off.ID}, 204)
	id = e.tracks(e.alice)[linked]["id"].(string)
	e.relocate(linked, "kept/one/y"+filepath.Ext(linked))
	litter("kept/one")
	e.json(e.alice, "DELETE", "/v2/tracks/"+id, nil, 204)
	e.relocate(dotted, "kept/two/z"+filepath.Ext(dotted))
	e.organize()
	if !exists(a("kept/one/cover.jpg")) || !exists(a("kept/two")) {
		t.Fatal("folders removed with deleteEmptyOriginals off")
	}

	old := time.Now().Add(-time.Hour)
	os.MkdirAll(a("empty/sub"), 0o755)
	os.WriteFile(a("empty/sub/.DS_Store"), []byte("x"), 0o644)
	os.MkdirAll(a("scans"), 0o755)
	os.WriteFile(a("scans/booklet.pdf"), []byte("x"), 0o644)
	os.MkdirAll(a("fresh"), 0o755)
	for _, d := range []string{"empty/sub", "empty", "scans"} {
		os.Chtimes(a(d), old, old)
	}
	e.server.Libs.Get(e.server.libID("alice")).Scan()
	for deadline := time.Now().Add(time.Minute); exists(a("empty")); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a scan left an empty folder")
		}
	}
	if !exists(a("scans/booklet.pdf")) || !exists(a("fresh")) || !exists(a("kept/one/cover.jpg")) {
		t.Fatal("a scan removed a folder that was new or not empty")
	}
}

func TestSessions(t *testing.T) {
	e := setup(t)
	sessions := func() Sessions {
		rec := httptest.NewRecorder()
		e.server.Sessions(rec, httptest.NewRequest("GET", "/?log", nil))
		var v Sessions
		json.Unmarshal(rec.Body.Bytes(), &v)
		return v
	}
	var ids []string
	for _, tr := range e.tracks(e.alice) {
		ids = append(ids, tr["id"].(string))
	}
	play := func(token, id, query string, headers map[string]string) int {
		res := e.do(token, "GET", "/v2/tracks/"+id+"/stream"+query, nil, headers)
		n, _ := io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return int(n)
	}
	n := play(e.alice, ids[0], "", nil)
	n += play(e.alice, ids[0], "", map[string]string{"Range": "bytes=0-99"})
	v := sessions()
	if len(v.Live) != 1 || len(v.Log) != 0 {
		t.Fatalf("after one track: %+v", v)
	}
	s := v.Live[0]
	if s.User != "alice" || s.Device != "Test" || s.Track != ids[0] || s.Quality != "original" || s.Bytes != int64(n) ||
		s.Requests != 2 || s.Sending != 0 || s.Ended.IsZero() || s.Offset != "bytes=0-99" || s.Title == "" || s.Path == "" {
		t.Fatalf("stream: %+v", s)
	}
	// The file endpoint (offline copies) is not a session, and a bad request is not recorded.
	e.do(e.alice, "GET", "/v2/tracks/"+ids[1]+"/file", nil, nil).Body.Close()
	play(e.alice, ids[1], "?quality=nonsense", nil)
	// A third track retires the oldest: the device has moved on.
	play(e.alice, ids[1], "", nil)
	play(e.alice, ids[2], "", nil)
	v = sessions()
	if len(v.Live) != 2 || len(v.Log) != 1 || v.Log[0].Track != ids[0] || v.Live[1].Track != ids[2] {
		t.Fatalf("after three tracks: %+v", v)
	}
	// Bob's stream is his own row.
	if play(e.bob, e.tracksOf(e.bob), "", nil); len(sessions().Live) != 3 {
		t.Fatal("bob's stream missing")
	}
}

func (e *env) tracksOf(token string) string {
	for _, tr := range e.tracks(token) {
		return tr["id"].(string)
	}
	return ""
}

// planCounts plans alice's library and counts ops by status.
func (e *env) planCounts() (map[string]any, map[string]int) {
	plan := e.json(e.alice, "POST", "/v2/organizer/plan", map[string]any{}, 200)
	n := map[string]int{}
	for _, op := range plan["operations"].([]any) {
		n[op.(map[string]any)["status"].(string)]++
	}
	return plan, n
}

// addCopy puts a copy of a test file into alice's library and waits for it.
func (e *env) addCopy(src, rel string) string {
	copyFile(e.t, "../testdata/audio/"+src, filepath.Join(e.aRoot, rel))
	e.server.Libs.Get(e.server.libID("alice")).Scan()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(100 * time.Millisecond) {
		if t, ok := e.tracks(e.alice)[rel]; ok {
			return t["id"].(string)
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s never indexed", rel)
		}
	}
}

// settles applies the organizer and checks the next two runs change nothing.
func (e *env) settles() {
	e.t.Helper()
	e.organize()
	var hash any
	for run := 2; run <= 3; run++ {
		plan, n := e.planCounts()
		if n["move"] != 0 || n["conflict"] != 0 {
			e.t.Fatalf("run %d plans %v", run, n)
		}
		res := e.json(e.alice, "POST", "/v2/organizer/apply", map[string]any{"planHash": plan["planHash"]}, 200)
		if res["moved"].(float64) != 0 {
			e.t.Fatalf("run %d moved %v", run, res["moved"])
		}
		if hash != nil && plan["planHash"] != hash {
			e.t.Fatalf("run %d plan hash changed", run)
		}
		hash = plan["planHash"]
	}
}

func TestOrganizerDuplicatesSettle(t *testing.T) {
	e := setup(t)
	ids := map[string]bool{e.tracks(e.alice)["cur-single.flac"]["id"].(string): true}
	ids[e.addCopy("cur-single.flac", "copy-a.flac")] = true
	ids[e.addCopy("cur-single.flac", "copy-b.flac")] = true
	plan, _ := e.planCounts()
	conflicts := 0
	for _, op := range plan["operations"].([]any) {
		if o := op.(map[string]any); o["status"] == "conflict" && ids[o["trackID"].(string)] {
			conflicts++
		}
	}
	if conflicts != 3 {
		t.Fatalf("first plan: %d conflicts among the copies", conflicts)
	}
	e.settles()
	numbered := 0
	for p, tr := range e.tracks(e.alice) {
		if ids[tr["id"].(string)] && (strings.HasSuffix(p, " (1).flac") || strings.HasSuffix(p, " (2).flac")) {
			numbered++
		}
	}
	if numbered != 2 {
		t.Fatalf("%d numbered copies", numbered)
	}
	// The moves alone don't make the next scan re-read or re-hash anything.
	seq := e.get(e.alice, "/v2/library", 200)["seq"].(float64)
	e.server.Libs.Get(e.server.libID("alice")).Scan()
	time.Sleep(3 * time.Second)
	e.waitReady(e.alice)
	if feed := e.get(e.alice, fmt.Sprintf("/v2/tracks?since=%d", int(seq)), 200); len(feed["tracks"].([]any)) != 0 {
		t.Fatalf("rescan after organizing touched %d tracks", len(feed["tracks"].([]any)))
	}
}

func TestOrganizerCaseAndNormalizationCollide(t *testing.T) {
	e := setup(t)
	for i, title := range []string{"Café", "CAFÉ", "Café"} {
		id := e.addCopy("cur-single.flac", fmt.Sprintf("copy-%d.flac", i))
		e.json(e.alice, "PATCH", "/v2/tracks/"+id, map[string]any{"title": title}, 200)
	}
	plan, n := e.planCounts()
	var tos []string
	for _, op := range plan["operations"].([]any) {
		if to := op.(map[string]any)["to"].(string); strings.Contains(strings.ToLower(to), "caf") {
			tos = append(tos, to)
		}
	}
	sort.Strings(tos)
	if len(tos) != 3 || !strings.HasSuffix(tos[0], " (1).flac") || !strings.HasSuffix(tos[1], " (2).flac") || n["conflict"] < 3 {
		t.Fatalf("plan %v: %v", n, tos)
	}
	e.settles()
}

func TestOrganizerOutsideFileBlocksOnce(t *testing.T) {
	e := setup(t)
	plan, _ := e.planCounts()
	var to string
	for _, op := range plan["operations"].([]any) {
		if o := op.(map[string]any); o["from"] == "cur-single.flac" {
			to = o["to"].(string)
		}
	}
	// A file the library doesn't track sits at the planned name.
	os.MkdirAll(filepath.Dir(filepath.Join(e.aRoot, to)), 0o755)
	os.WriteFile(filepath.Join(e.aRoot, to), []byte("not audio"), 0o644)
	plan, _ = e.planCounts()
	for _, op := range plan["operations"].([]any) {
		if o := op.(map[string]any); o["from"] == "cur-single.flac" && (o["status"] != "conflict" || o["to"] != strings.TrimSuffix(to, ".flac")+" (1).flac") {
			t.Fatalf("blocked op: %v", o)
		}
	}
	e.settles()
	if b, _ := os.ReadFile(filepath.Join(e.aRoot, to)); string(b) != "not audio" {
		t.Fatal("outside file replaced")
	}
}

func TestScanSeesInPlaceRewrite(t *testing.T) {
	e := setup(t)
	p := filepath.Join(e.aRoot, "cur-single.flac")
	before, _ := os.Stat(p)
	old := e.tracks(e.alice)["cur-single.flac"]["contentHash"]
	// Rewrite the last byte in place, then put mtime back, as dd conv=notrunc
	// followed by touch -r would: size, mtime and inode all stay the same.
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := make([]byte, 1)
	f.ReadAt(last, before.Size()-1)
	f.WriteAt([]byte{last[0] ^ 0xFF}, before.Size()-1)
	f.Close()
	os.Chtimes(p, before.ModTime(), before.ModTime())
	data, _ := os.ReadFile(p)
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	if old == want {
		t.Fatal("rewrite changed nothing")
	}
	e.server.Libs.Get(e.server.libID("alice")).Scan()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if got := e.tracks(e.alice)["cur-single.flac"]["contentHash"]; got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("contentHash %v, want %s (was %v)", got, want, old)
		}
	}
}

func TestOrganizerSwapsThroughTemporaryNames(t *testing.T) {
	e := setup(t)
	e.organize()
	tracks := e.tracks(e.alice)
	var p1, p2 string
	for p := range tracks {
		if strings.HasSuffix(p, ".flac") && (p1 == "" || p < p1) {
			p1 = p
		}
	}
	for p := range tracks {
		if strings.HasSuffix(p, ".flac") && p != p1 && path.Dir(p) != path.Dir(p1) && (p2 == "" || p < p2) {
			p2 = p
		}
	}
	// The scanner gives swapped files new IDs (each ID is still claimed by a
	// live path), so follow them by title.
	t1, t2 := tracks[p1]["title"], tracks[p2]["title"]
	// Swap the two files on disk: each now sits at the other's planned name.
	a, b := filepath.Join(e.aRoot, p1), filepath.Join(e.aRoot, p2)
	tmp := a + ".swap"
	for _, mv := range [][2]string{{a, tmp}, {b, a}, {tmp, b}} {
		if err := os.Rename(mv[0], mv[1]); err != nil {
			t.Fatal(err)
		}
	}
	e.server.Libs.Get(e.server.libID("alice")).Scan()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(100 * time.Millisecond) {
		now := e.tracks(e.alice)
		if now[p1]["title"] == t2 && now[p2]["title"] == t1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan never followed the swap")
		}
	}
	if _, n := e.planCounts(); n["move"] != 2 {
		t.Fatalf("swap plans %v", n)
	}
	e.settles()
	if now := e.tracks(e.alice); now[p1]["title"] != t1 || now[p2]["title"] != t2 {
		t.Fatalf("not swapped back: %v %v", now[p1]["title"], now[p2]["title"])
	}
	entries, _ := os.ReadDir(filepath.Dir(a))
	for _, en := range entries {
		if strings.Contains(en.Name(), "(organizing ") {
			t.Fatalf("temporary name left behind: %s", en.Name())
		}
	}
}
