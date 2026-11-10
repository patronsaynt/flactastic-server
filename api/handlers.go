package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"flactastic/server/library"
	"flactastic/server/meta"
	"flactastic/server/store"
	"flactastic/server/taglib"
)

func (srv *Server) server(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"id": srv.Store.Setting("id"), "name": srv.Name, "protocol": "2.0",
		"version": srv.Version, "transcoding": srv.FFmpeg != ""})
}

// Accounts (protocol §6)

type deviceJSON struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Created  meta.Time `json:"created"`
	LastSeen meta.Time `json:"lastSeen"`
	Current  bool      `json:"current"`
}

func deviceOut(d store.Device, current string) deviceJSON {
	return deviceJSON{d.ID, d.Name, d.Kind, meta.Time{Time: d.Created}, meta.Time{Time: d.Seen}, d.ID == current}
}

func userOut(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "name": u.Name, "isAdmin": u.Admin}
}

var deviceKinds = []string{"mac", "iPhone", "iPad", "windows", "linux", "other"}

func (srv *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password, DeviceName, DeviceKind string }
	if err := decode(w, r, jsonLimit, &in); err != nil {
		fail(w, err)
		return
	}
	if in.DeviceName == "" || len(in.DeviceName) > 63 || !utf8.ValidString(in.DeviceName) || !slices.Contains(deviceKinds, in.DeviceKind) {
		fail(w, errBadRequest)
		return
	}
	if !store.NamePattern.MatchString(in.Username) {
		in.Username = "" // never a real user; keeps limiter keys bounded
	}
	ipKey, userKey := "ip:"+clientIP(r), "user:"+in.Username
	if d := srv.failures.wait(ipKey, userKey); d > 0 {
		retry(w, d)
		return
	}
	user, good := srv.Store.Authenticate(in.Username, in.Password)
	if !good {
		srv.failures.fail(ipKey, userKey)
		fail(w, errCredentials)
		return
	}
	srv.failures.clear(userKey)
	d, token, err := srv.Store.AddDevice(user.ID, in.DeviceName, in.DeviceKind)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, map[string]any{"token": token, "device": deviceOut(d, d.ID), "user": userOut(user)})
}

func (srv *Server) logout(w http.ResponseWriter, r *http.Request, s session) error {
	if err := srv.Store.RevokeDevice(s.device.ID, s.user.ID); err != nil {
		return err
	}
	return noContent(w)
}

func (srv *Server) me(w http.ResponseWriter, r *http.Request, s session) error {
	return ok(w, map[string]any{"user": userOut(s.user), "device": deviceOut(s.device, s.device.ID)})
}

func (srv *Server) password(w http.ResponseWriter, r *http.Request, s session) error {
	var in struct{ Current, New string }
	if err := decode(w, r, jsonLimit, &in); err != nil {
		return err
	}
	key := "user:" + s.user.Name
	if d := srv.failures.wait(key); d > 0 {
		retry(w, d)
		return nil
	}
	if _, good := srv.Store.Authenticate(s.user.Name, in.Current); !good {
		srv.failures.fail(key)
		return errCredentials
	}
	if store.CheckPassword(in.New) != nil {
		return errBadRequest
	}
	if err := srv.Store.SetUser(s.user.Name, "hash", in.New); err != nil {
		return err
	}
	if err := srv.Store.RevokeOthers(s.user.ID, s.device.ID); err != nil {
		return err
	}
	return noContent(w)
}

func (srv *Server) devices(w http.ResponseWriter, r *http.Request, s session) error {
	list, err := srv.Store.Devices(s.user.ID)
	if err != nil {
		return err
	}
	out := []deviceJSON{}
	for _, d := range list {
		out = append(out, deviceOut(d, s.device.ID))
	}
	return ok(w, map[string]any{"devices": out})
}

func (srv *Server) revoke(w http.ResponseWriter, r *http.Request, s session) error {
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	if err := srv.Store.RevokeDevice(id.String(), s.user.ID); err != nil {
		return err
	}
	return noContent(w)
}

// Library (protocol §7)

func (srv *Server) library(w http.ResponseWriter, r *http.Request, s session) error {
	st := s.lib.Stats()
	return ok(w, map[string]any{"libraryID": s.user.Library, "state": s.lib.State(), "seq": s.lib.Seq(),
		"trackCount": st.TrackCount, "pendingHashes": st.PendingHashes, "usedBytes": st.UsedBytes,
		"quotaBytes": s.user.Quota, "freeBytes": st.FreeBytes})
}

func (srv *Server) tracks(w http.ResponseWriter, r *http.Request, s session) error {
	since, err := queryInt(r, "since", 0, 0, 1<<53)
	if err != nil {
		return err
	}
	limit, err := queryInt(r, "limit", 1000, 1, 5000)
	if err != nil {
		return err
	}
	tracks, deleted, seq, more, err := s.lib.TracksSince(since, int(limit))
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"tracks": orEmpty(tracks), "deleted": orEmpty(deleted), "seq": seq, "more": more})
}

func orEmpty[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

func (srv *Server) track(w http.ResponseWriter, r *http.Request, s session) error {
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	t, cue, lyrics, err := s.lib.Detail(id)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(t)
	var out map[string]any
	json.Unmarshal(b, &out)
	if cue != "" {
		out["cueSheet"] = cue
	}
	if lyrics != "" {
		out["lyrics"] = lyrics
	}
	return ok(w, out)
}

func (srv *Server) albums(w http.ResponseWriter, r *http.Request, s session) error {
	albums, err := s.lib.Albums()
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"albums": orEmpty(albums)})
}

func (srv *Server) search(w http.ResponseWriter, r *http.Request, s session) error {
	limit, err := queryInt(r, "limit", 50, 1, 200)
	if err != nil {
		return err
	}
	tracks, err := s.lib.Search(r.URL.Query().Get("q"), int(limit))
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"tracks": orEmpty(tracks)})
}

// Tag edits (protocol §10)

const (
	maxArtwork = 10 << 20
	editLimit  = 16 << 20 // room for base64 artwork
)

func (srv *Server) edit(w http.ResponseWriter, r *http.Request, s session) error {
	var ids []meta.UUID
	var e taglib.Edit
	if r.PathValue("id") != "" {
		id, err := pathUUID(r)
		if err != nil {
			return err
		}
		ids = []meta.UUID{id}
		if err := decode(w, r, editLimit, &e); err != nil {
			return err
		}
	} else {
		var in struct {
			IDs  []meta.UUID `json:"ids"`
			Edit taglib.Edit `json:"edit"`
		}
		if err := decode(w, r, editLimit, &in); err != nil {
			return err
		}
		if len(in.IDs) == 0 || len(in.IDs) > 1000 {
			return errBadRequest
		}
		ids, e = in.IDs, in.Edit
	}
	if e.Artwork.V != nil && (len(*e.Artwork.V) > maxArtwork || !library.ValidImage(*e.Artwork.V)) {
		return errBadRequest
	}
	var prefs library.Prefs
	s.lib.Doc("prefs", &prefs)
	tracks, err := s.lib.Edit(ids, e, prefs.Separator())
	if err != nil {
		return err
	}
	if r.PathValue("id") != "" {
		return ok(w, map[string]any{"track": tracks[0]})
	}
	return ok(w, map[string]any{"tracks": tracks})
}

func (srv *Server) deleteTrack(w http.ResponseWriter, r *http.Request, s session) error {
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	if err := s.lib.Delete(id); err != nil {
		return err
	}
	return noContent(w)
}

// Uploads (protocol §9.4)

func (srv *Server) createUpload(w http.ResponseWriter, r *http.Request, s session) error {
	var in struct {
		Filename    string     `json:"filename"`
		Size        int64      `json:"size"`
		ContentHash string     `json:"contentHash"`
		TrackID     *meta.UUID `json:"trackID"`
	}
	if err := decode(w, r, jsonLimit, &in); err != nil {
		return err
	}
	u, err := s.lib.CreateUpload(in.Filename, in.Size, in.ContentHash, in.TrackID)
	if err != nil {
		return err
	}
	return reply(w, http.StatusCreated, u)
}

func (srv *Server) uploadStatus(w http.ResponseWriter, r *http.Request, s session) error {
	u, err := s.lib.UploadStatus(uploadID(r))
	if err != nil {
		return err
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(u.Offset, 10))
	return noContent(w)
}

func (srv *Server) appendUpload(w http.ResponseWriter, r *http.Request, s session) error {
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		return errBadRequest
	}
	u, t, err := s.lib.AppendUpload(uploadID(r), offset, r.Body)
	w.Header().Set("Upload-Offset", strconv.FormatInt(u.Offset, 10))
	if err != nil {
		return err
	}
	if t != nil {
		return ok(w, map[string]any{"track": t})
	}
	return noContent(w)
}

func (srv *Server) cancelUpload(w http.ResponseWriter, r *http.Request, s session) error {
	if err := s.lib.CancelUpload(uploadID(r)); err != nil {
		return err
	}
	return noContent(w)
}

// uploadID reads an upload ID in any case; an unparsable one names nothing.
func uploadID(r *http.Request) string {
	id, err := meta.ParseUUID(r.PathValue("id"))
	if err != nil {
		return ""
	}
	return id.String()
}

// Change signal (protocol §11)

func (srv *Server) changes(w http.ResponseWriter, r *http.Request, s session) error {
	since, err := queryInt(r, "since", 0, 0, 1<<53)
	if err != nil {
		return err
	}
	wait, err := queryInt(r, "wait", 0, 0, 60)
	if err != nil {
		return err
	}
	seq, kinds := s.lib.Wait(r.Context(), since, time.Duration(wait)*time.Second)
	return ok(w, map[string]any{"seq": seq, "changed": orEmpty(kinds)})
}

// Offline copies (protocol §13)

func (srv *Server) manifest(w http.ResponseWriter, r *http.Request, s session) error {
	var in struct {
		Selection *library.Selection `json:"selection"`
		Filter    *meta.SyncFilter   `json:"filter"`
	}
	if err := decode(w, r, largeLimit, &in); err != nil {
		return err
	}
	filter := meta.Unrestricted
	if in.Filter != nil {
		filter = *in.Filter
	}
	m, pending, err := s.lib.Manifest(in.Selection, filter)
	if err != nil {
		return err
	}
	w.Header().Set("FLACtastic-Pending", strconv.Itoa(pending))
	return ok(w, m)
}

// Organizer (protocol §12)

func (srv *Server) organizer(w http.ResponseWriter, r *http.Request, s session) error {
	o := library.Organizer{Profiles: []meta.Profile{}}
	if err := s.lib.Doc("organizer", &o); err != nil {
		return err
	}
	return ok(w, o)
}

func (srv *Server) putOrganizer(w http.ResponseWriter, r *http.Request, s session) error {
	var o library.Organizer
	if err := decode(w, r, jsonLimit, &o); err != nil {
		return err
	}
	if len(o.Profiles) > 100 {
		return errBadRequest
	}
	if err := s.lib.SetDoc("organizer", library.KindOrganizer, o); err != nil {
		return err
	}
	return noContent(w)
}

type planRequest struct {
	ProfileID *meta.UUID `json:"profileID"`
	PlanHash  string     `json:"planHash"`
}

func (srv *Server) organizerPlan(w http.ResponseWriter, r *http.Request, s session) error {
	var in planRequest
	if err := decode(w, r, jsonLimit, &in); err != nil {
		return err
	}
	hash, ops, err := s.lib.OrganizePlan(in.ProfileID)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"planHash": hash, "operations": orEmpty(ops)})
}

func (srv *Server) organizerApply(w http.ResponseWriter, r *http.Request, s session) error {
	var in planRequest
	if err := decode(w, r, jsonLimit, &in); err != nil {
		return err
	}
	moved, unchanged, err := s.lib.OrganizeApply(in.ProfileID, in.PlanHash)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"moved": moved, "unchanged": unchanged})
}

// Playlists (protocol §14)

func (srv *Server) playlists(w http.ResponseWriter, r *http.Request, s session) error {
	list, err := s.lib.Playlists()
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"playlists": list})
}

func (srv *Server) playlist(w http.ResponseWriter, r *http.Request, s session) error {
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	p, err := s.lib.Playlist(id)
	if err != nil {
		return err
	}
	return ok(w, p)
}

func (srv *Server) putPlaylist(w http.ResponseWriter, r *http.Request, s session) error {
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	var p meta.Playlist
	if err := decode(w, r, largeLimit, &p); err != nil {
		return err
	}
	if p.ID != id || utf8.RuneCountInString(p.Name) > 255 || len(p.Entries) > 10000 || len(p.CustomArtwork) > 5<<20 ||
		(p.Description != nil && utf8.RuneCountInString(*p.Description) > 200) {
		return errBadRequest
	}
	if err := s.lib.PutPlaylist(p); err != nil {
		return err
	}
	return noContent(w)
}

func (srv *Server) deletePlaylist(w http.ResponseWriter, r *http.Request, s session) error {
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	if err := s.lib.DeletePlaylist(id); err != nil {
		return err
	}
	return noContent(w)
}

// Preferences (protocol §14)

func (srv *Server) prefs(w http.ResponseWriter, r *http.Request, s session) error {
	var p library.Prefs
	if err := s.lib.Doc("prefs", &p); err != nil {
		return err
	}
	return ok(w, p.Filled())
}

func (srv *Server) patchPrefs(w http.ResponseWriter, r *http.Request, s session) error {
	var in library.Prefs
	if err := decode(w, r, largeLimit, &in); err != nil {
		return err
	}
	p, err := s.lib.PatchPrefs(in)
	if err != nil {
		return errBadRequest
	}
	return ok(w, p)
}

// User data (protocol §14)

func (srv *Server) userItems(w http.ResponseWriter, r *http.Request, s session) error {
	items, err := s.lib.UserItems()
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"items": items})
}

func (srv *Server) userItem(w http.ResponseWriter, r *http.Request, s session) error {
	data, etag, err := s.lib.UserItem(r.PathValue("name"))
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "private, no-cache")
	h.Set("ETag", `"`+etag+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	return nil
}

func (srv *Server) putUserItem(w http.ResponseWriter, r *http.Request, s session) error {
	name := r.PathValue("name")
	if !library.ValidUserName(name) {
		return errBadRequest
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, library.MaxUserItem))
	if err != nil {
		return err
	}
	etag, err := s.lib.PutUserItem(name, data)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	return noContent(w)
}

func (srv *Server) deleteUserItem(w http.ResponseWriter, r *http.Request, s session) error {
	if err := s.lib.DeleteUserItem(r.PathValue("name")); err != nil {
		return err
	}
	return noContent(w)
}

// History and stats (protocol §15)

func (srv *Server) addHistory(w http.ResponseWriter, r *http.Request, s session) error {
	var in struct {
		Events   []library.PlayEvent     `json:"events"`
		Contexts []library.RecentContext `json:"contexts"`
	}
	if err := decode(w, r, largeLimit, &in); err != nil {
		return err
	}
	if len(in.Events) > 1000 || len(in.Contexts) > 1000 {
		return errBadRequest
	}
	for _, c := range in.Contexts {
		if c.Kind != "album" && c.Kind != "playlist" {
			return errBadRequest
		}
	}
	n, err := s.lib.AddHistory(in.Events, in.Contexts)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"accepted": n})
}

func (srv *Server) history(w http.ResponseWriter, r *http.Request, s session) error {
	since, err := queryInt(r, "since", 0, 0, 1<<53)
	if err != nil {
		return err
	}
	limit, err := queryInt(r, "limit", 1000, 1, 5000)
	if err != nil {
		return err
	}
	events, contexts, seq, more, err := s.lib.HistorySince(since, int(limit))
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"events": orEmpty(events), "contexts": orEmpty(contexts), "seq": seq, "more": more})
}

func (srv *Server) stats(w http.ResponseWriter, r *http.Request, s session) error {
	q := r.URL.Query()
	loc, err := time.LoadLocation(q.Get("tz"))
	if err != nil {
		return errBadRequest
	}
	var since *time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return errBadRequest
		}
		since = &t
	}
	limit, err := queryInt(r, "limit", 10, 1, 50)
	if err != nil {
		return err
	}
	st, err := s.lib.Listening(loc, since, int(limit), time.Now())
	if err != nil {
		return err
	}
	return ok(w, st)
}

// Administration (protocol §16)

func (srv *Server) adminStatus(w http.ResponseWriter, r *http.Request, s session) error {
	if !s.user.Admin {
		return errForbidden
	}
	var users []map[string]any
	for _, l := range srv.Libs.All() {
		st := l.Stats()
		users = append(users, map[string]any{"name": l.User.Name, "state": l.State(), "trackCount": st.TrackCount,
			"pendingHashes": st.PendingHashes, "usedBytes": st.UsedBytes, "quotaBytes": l.User.Quota})
	}
	return ok(w, map[string]any{"version": srv.Version, "uptime": int(time.Since(srv.started).Seconds()),
		"users": orEmpty(users)})
}
