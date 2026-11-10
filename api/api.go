// Package api serves Server Protocol v2 (docs/server-protocol-v2.md).
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"flactastic/server/library"
	"flactastic/server/meta"
	"flactastic/server/store"
)

const (
	jsonLimit  = 1 << 20
	largeLimit = 8 << 20
)

type Server struct {
	Store      *store.Store
	Libs       *library.Manager
	Version    string
	Name       string
	FFmpeg     string // empty when transcoding is unavailable
	started    time.Time
	transcodes chan struct{}
	failures   limiter
}

func New(s *store.Store, libs *library.Manager, version, name, ffmpeg string) *Server {
	return &Server{Store: s, Libs: libs, Version: version, Name: name, FFmpeg: ffmpeg, started: time.Now(),
		transcodes: make(chan struct{}, runtime.NumCPU()), failures: limiter{m: map[string]*failure{}}}
}

// session is an authenticated request's identity. Everything a handler can
// reach hangs off it, so no request can name another user's data.
type session struct {
	user   store.User
	device store.Device
	lib    *library.Library
}

type handler func(w http.ResponseWriter, r *http.Request, s session) error

func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	open := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	auth := func(pattern string, h handler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) { srv.authed(w, r, h) })
	}
	open("GET /v2/server", srv.server)
	open("POST /v2/auth/login", srv.login)
	auth("POST /v2/auth/logout", srv.logout)
	auth("GET /v2/me", srv.me)
	auth("PUT /v2/me/password", srv.password)
	auth("GET /v2/devices", srv.devices)
	auth("DELETE /v2/devices/{id}", srv.revoke)
	auth("GET /v2/library", srv.library)
	auth("GET /v2/tracks", srv.tracks)
	auth("GET /v2/tracks/{id}", srv.track)
	auth("PATCH /v2/tracks/{id}", srv.edit)
	auth("PATCH /v2/tracks", srv.edit)
	auth("DELETE /v2/tracks/{id}", srv.deleteTrack)
	auth("GET /v2/tracks/{id}/file", srv.file)
	auth("GET /v2/tracks/{id}/stream", srv.stream)
	auth("GET /v2/art/{id}", srv.art)
	auth("GET /v2/albums", srv.albums)
	auth("GET /v2/search", srv.search)
	auth("POST /v2/uploads", srv.createUpload)
	auth("HEAD /v2/uploads/{id}", srv.uploadStatus)
	auth("PATCH /v2/uploads/{id}", srv.appendUpload)
	auth("DELETE /v2/uploads/{id}", srv.cancelUpload)
	auth("GET /v2/changes", srv.changes)
	auth("POST /v2/manifest", srv.manifest)
	auth("GET /v2/organizer", srv.organizer)
	auth("PUT /v2/organizer", srv.putOrganizer)
	auth("POST /v2/organizer/plan", srv.organizerPlan)
	auth("POST /v2/organizer/apply", srv.organizerApply)
	auth("GET /v2/playlists", srv.playlists)
	auth("GET /v2/playlists/{id}", srv.playlist)
	auth("PUT /v2/playlists/{id}", srv.putPlaylist)
	auth("DELETE /v2/playlists/{id}", srv.deletePlaylist)
	auth("GET /v2/prefs", srv.prefs)
	auth("PATCH /v2/prefs", srv.patchPrefs)
	auth("POST /v2/history", srv.addHistory)
	auth("GET /v2/history", srv.history)
	auth("GET /v2/stats", srv.stats)
	auth("GET /v2/admin/status", srv.adminStatus)
	open("/", func(w http.ResponseWriter, r *http.Request) { fail(w, errNotFound) })
	return headers(mux)
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Errors (protocol §5)

type apiError struct {
	status int
	code   string
}

func (e *apiError) Error() string { return e.code }

func newErr(status int, code string) *apiError { return &apiError{status, code} }

var (
	errBadRequest  = newErr(400, "badRequest")
	errFormat      = newErr(400, "unsupportedFormat")
	errAuth        = newErr(401, "unauthorized")
	errCredentials = newErr(401, "invalidCredentials")
	errForbidden   = newErr(403, "forbidden")
	errNotFound    = newErr(404, "notFound")
	errOffset      = newErr(409, "offsetMismatch")
	errPlanStale   = newErr(409, "planStale")
	errTooLarge    = newErr(413, "sizeExceeded")
	errHash        = newErr(422, "hashMismatch")
	errRateLimited = newErr(429, "rateLimited")
	errOffline     = newErr(503, "libraryOffline")
	errTranscode   = newErr(503, "transcodeUnavailable")
	errStorage     = newErr(507, "insufficientStorage")
	errInternal    = newErr(500, "internalFailure")
)

func fail(w http.ResponseWriter, err error) {
	var e *apiError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &e):
	case errors.As(err, &tooBig):
		e = errTooLarge
	case errors.Is(err, library.ErrNotFound), errors.Is(err, store.ErrNotFound), errors.Is(err, library.ErrEscape),
		errors.Is(err, os.ErrNotExist):
		e = errNotFound
	case errors.Is(err, library.ErrOffline):
		e = errOffline
	case errors.Is(err, library.ErrUnsupported):
		e = errFormat
	case errors.Is(err, library.ErrTooLarge):
		e = errTooLarge
	case errors.Is(err, library.ErrNoSpace):
		e = errStorage
	case errors.Is(err, library.ErrOffset):
		e = errOffset
	case errors.Is(err, library.ErrInvalid):
		e = errBadRequest
	case errors.Is(err, library.ErrHash):
		e = errHash
	case errors.Is(err, library.ErrPlanStale):
		e = errPlanStale
	default:
		slog.Error("request failed", "err", err)
		e = errInternal
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": e.code, "message": e.code}})
}

func reply(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, v any) error { return reply(w, http.StatusOK, v) }

func noContent(w http.ResponseWriter) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return errTooLarge
		}
		return errBadRequest
	}
	return nil
}

func pathUUID(r *http.Request) (meta.UUID, error) {
	id, err := meta.ParseUUID(r.PathValue("id"))
	if err != nil {
		return id, errNotFound
	}
	return id, nil
}

func queryInt(r *http.Request, key string, def, lo, hi int64) (int64, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, errBadRequest
	}
	return n, nil
}

// Sign-in back-off (protocol §6): 5 free failures per IP and per username,
// then 1 s doubling to 15 minutes.

type failure struct {
	count int
	until time.Time
}

type limiter struct {
	mu sync.Mutex
	m  map[string]*failure
}

func (l *limiter) wait(keys ...string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	var d time.Duration
	for _, k := range keys {
		if f := l.m[k]; f != nil {
			d = max(d, time.Until(f.until))
		}
	}
	return d
}

func (l *limiter) fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 100000 {
		for k, f := range l.m {
			if time.Since(f.until) > 15*time.Minute {
				delete(l.m, k)
			}
		}
	}
	for _, k := range keys {
		f := l.m[k]
		if f == nil {
			f = &failure{}
			l.m[k] = f
		}
		f.count++
		if f.count >= 5 {
			f.until = time.Now().Add(min(time.Second<<min(f.count-5, 20), 15*time.Minute))
		}
	}
}

func (l *limiter) clear(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (srv *Server) authed(w http.ResponseWriter, r *http.Request, h handler) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || token == "" {
		fail(w, errAuth)
		return
	}
	if d := srv.failures.wait("ip:" + clientIP(r)); d > 0 {
		retry(w, d)
		return
	}
	device, user, err := srv.Store.DeviceForToken(token)
	if err != nil {
		srv.failures.fail("ip:" + clientIP(r))
		fail(w, errAuth)
		return
	}
	srv.Store.Touch(device)
	lib := srv.Libs.Get(user.ID)
	if lib == nil {
		fail(w, errOffline)
		return
	}
	if err := h(w, r, session{user: user, device: device, lib: lib}); err != nil {
		fail(w, err)
	}
}

func retry(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
	fail(w, errRateLimited)
}
