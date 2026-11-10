package api

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"

	"flactastic/server/library"
	"flactastic/server/meta"
)

var contentTypes = map[meta.Format]string{meta.FLAC: "audio/flac", meta.MP3: "audio/mpeg", meta.WAV: "audio/wav",
	meta.AIFF: "audio/aiff", meta.ALAC: "audio/mp4", meta.AAC: "audio/mp4"}

func (srv *Server) openTrack(r *http.Request, s session) (library.Track, *os.File, error) {
	id, err := pathUUID(r)
	if err != nil {
		return library.Track{}, nil, err
	}
	t, err := s.lib.Track(id)
	if err != nil {
		return t, nil, errNotFound
	}
	f, err := s.lib.OpenTrack(t)
	return t, f, err
}

// file serves the original bytes with full range and validator support.
func (srv *Server) file(w http.ResponseWriter, r *http.Request, s session) error {
	t, f, err := srv.openTrack(r, s)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	h := w.Header()
	ct := contentTypes[t.Format]
	if t.Format == meta.AAC && len(t.RelativePath) > 4 && t.RelativePath[len(t.RelativePath)-4:] == ".aac" {
		ct = "audio/aac"
	}
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "private, no-cache")
	if t.ContentHash != nil && info.Size() == t.FileSize {
		h.Set("ETag", `"sha256:`+*t.ContentHash+`"`)
	} else {
		h.Set("ETag", fmt.Sprintf(`"m:%d-%d"`, info.Size(), info.ModTime().UnixNano()))
	}
	http.ServeContent(w, r, "", info.ModTime(), f)
	return nil
}

var bitrates = map[string]string{"high": "256k", "normal": "160k", "low": "96k"}

const aacPriming = 1024

func outputRate(rate int) int {
	switch {
	case rate <= 48000:
		return rate
	case rate%44100 == 0:
		return 44100
	default:
		return 48000
	}
}

// stream serves the original, or an AAC transcode with the gapless trim
// headers of protocol §9.2.
func (srv *Server) stream(w http.ResponseWriter, r *http.Request, s session) error {
	q := r.URL.Query().Get("quality")
	if q == "" || q == "original" {
		return srv.file(w, r, s)
	}
	bitrate, known := bitrates[q]
	if !known {
		return errBadRequest
	}
	if srv.FFmpeg == "" {
		return errTranscode
	}
	t, f, err := srv.openTrack(r, s)
	if err != nil {
		return err
	}
	defer f.Close()
	if t.SampleRate <= 0 {
		return errTranscode
	}
	limit := int64(t.Frames)
	if limit == 0 { // length unknown
		limit = 1 << 40
	}
	start, err := queryInt(r, "s", 0, 0, limit)
	if err != nil {
		return err
	}
	select {
	case srv.transcodes <- struct{}{}:
		defer func() { <-srv.transcodes }()
	default:
		return errTranscode
	}
	rate, out := int64(t.SampleRate), outputRate(t.SampleRate)
	// Seek to a whole second before the target, which ffmpeg lands on
	// exactly, then trim the remaining samples.
	seek := max(0, start/rate-1)
	af := fmt.Sprintf("atrim=start_sample=%d,asetpts=PTS-STARTPTS,aresample=%d", start-seek*rate, out)
	cmd := exec.CommandContext(r.Context(), srv.FFmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatInt(seek, 10), "-i", "/dev/fd/3", "-map", "0:a:0", "-af", af,
		"-ac", strconv.Itoa(min(max(t.Channels, 1), 2)), "-c:a", "aac", "-b:a", bitrate, "-f", "adts", "pipe:1")
	cmd.ExtraFiles = []*os.File{f}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "audio/aac")
	h.Set("Accept-Ranges", "none")
	h.Set("FLACtastic-Sample-Rate", strconv.Itoa(out))
	h.Set("FLACtastic-Priming", strconv.Itoa(aacPriming))
	if t.Frames > 0 {
		h.Set("FLACtastic-Frames", strconv.FormatInt((int64(t.Frames)-start)*int64(out)/rate, 10))
	}
	w.WriteHeader(http.StatusOK)
	io.Copy(w, stdout)
	if err := cmd.Wait(); err != nil && r.Context().Err() == nil {
		slog.Warn("transcode failed", "err", err, "ffmpeg", stderr.String())
	}
	return nil
}

// art serves embedded pictures and cached thumbnails (protocol §9.3).
func (srv *Server) art(w http.ResponseWriter, r *http.Request, s session) error {
	id := r.PathValue("id")
	size := r.URL.Query().Get("size")
	if size == "" {
		size = "original"
	}
	if len(id) != 32 {
		return errNotFound
	}
	h := w.Header()
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	h.Set("ETag", `"`+id+"-"+size+`"`)
	switch size {
	case "original":
		pic, err := s.lib.Picture(id)
		if err != nil {
			return errNotFound
		}
		ct := "image/jpeg"
		if bytes.HasPrefix(pic, []byte("\x89PNG")) {
			ct = "image/png"
		}
		h.Set("Content-Type", ct)
		http.ServeContent(w, r, "", srv.started, bytes.NewReader(pic))
	case "300", "1000":
		n, _ := strconv.Atoi(size)
		file, err := s.lib.Thumbnail(id, n)
		if err != nil {
			return errNotFound
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		h.Set("Content-Type", "image/jpeg")
		http.ServeContent(w, r, "", srv.started, f)
	default:
		return errBadRequest
	}
	return nil
}
