package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"flactastic/server/library"
)

// Stream is one device fetching one track at one quality, across however
// many requests that takes. It is what the session command shows.
type Stream struct {
	ID                             int64
	User, Device, DeviceID, Kind   string
	Addr, Proto, Agent             string // of the latest request
	Track, Title, Artist, Path     string
	Format, Quality, Offset, Error string // Offset is the latest Range or seek
	SampleRate, BitDepth           int
	Duration                       float64
	Started, Last, Ended           time.Time // first request, latest request, when sending stopped
	Bytes                          int64
	Requests, Cancelled, Sending   int
}

// Sessions is the admin socket's answer.
type Sessions struct {
	Live, Log         []Stream
	Transcodes, Slots int
}

const (
	streamLog    = 500
	streamLinger = 30 * time.Second
)

type streams struct {
	mu   sync.Mutex
	next int64
	live []*Stream
	log  []Stream // retired, oldest first
}

// retire moves idle streams to the log: those whose track has had time to
// play out, and all but each device's two newest (the track playing and the
// one preloaded), which is how a skip shows. Callers hold mu.
func (ss *streams) retire() {
	left := map[string]int{}
	for _, st := range ss.live {
		left[st.DeviceID]++
	}
	keep := ss.live[:0]
	for _, st := range ss.live {
		left[st.DeviceID]--
		length := time.Duration(st.Duration*float64(time.Second)) + streamLinger
		if st.Sending == 0 && (left[st.DeviceID] >= 2 || time.Since(st.Last) > length) {
			ss.log = append(ss.log, *st)
			continue
		}
		keep = append(keep, st)
	}
	clear(ss.live[len(keep):])
	ss.live = keep
	if n := len(ss.log) - streamLog; n > 0 {
		ss.log = ss.log[n:]
	}
}

type counted struct {
	http.ResponseWriter
	add func(int)
}

func (c counted) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.add(n)
	return n, err
}

func (c counted) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// watch records a stream request. It returns the writer to answer through
// and a func to call with the request's outcome.
func (srv *Server) watch(w http.ResponseWriter, r *http.Request, s session, t library.Track, quality string) (http.ResponseWriter, func(error)) {
	ss := &srv.streams
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var st *Stream
	for _, x := range ss.live {
		if x.DeviceID == s.device.ID && x.Track == t.ID.String() && x.Quality == quality {
			st = x
		}
	}
	if st == nil {
		ss.next++
		st = &Stream{ID: ss.next, User: s.user.Name, Device: s.device.Name, DeviceID: s.device.ID, Kind: s.device.Kind,
			Track: t.ID.String(), Title: t.Title, Path: t.RelativePath, Format: string(t.Format), Quality: quality,
			SampleRate: t.SampleRate, Duration: t.Duration, Started: time.Now()}
		if t.Artist != nil {
			st.Artist = *t.Artist
		}
		if t.BitDepth != nil {
			st.BitDepth = *t.BitDepth
		}
		ss.live = append(ss.live, st)
	}
	st.Addr, st.Proto, st.Agent, st.Last, st.Ended = clientIP(r), r.Proto, r.UserAgent(), time.Now(), time.Time{}
	if st.Offset = r.Header.Get("Range"); r.URL.Query().Get("s") != "" {
		st.Offset = "sample " + r.URL.Query().Get("s")
	}
	st.Requests++
	st.Sending++
	ss.retire()
	add := func(n int) {
		ss.mu.Lock()
		st.Bytes += int64(n)
		ss.mu.Unlock()
	}
	return counted{w, add}, func(err error) {
		ss.mu.Lock()
		defer ss.mu.Unlock()
		if st.Sending--; st.Sending == 0 {
			st.Ended = time.Now()
		}
		if err != nil {
			st.Error = err.Error()
		} else if r.Context().Err() != nil {
			st.Cancelled++
		}
	}
}

// Sessions answers the session command on the admin socket.
func (srv *Server) Sessions(w http.ResponseWriter, r *http.Request) {
	ss := &srv.streams
	ss.mu.Lock()
	ss.retire()
	out := Sessions{Live: []Stream{}, Transcodes: len(srv.transcodes), Slots: cap(srv.transcodes)}
	for _, st := range ss.live {
		out.Live = append(out.Live, *st)
	}
	if r.URL.Query().Has("log") {
		out.Log = append(out.Log, ss.log...)
	}
	ss.mu.Unlock()
	json.NewEncoder(w).Encode(out)
}
