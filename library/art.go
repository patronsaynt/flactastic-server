package library

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/draw"
	"golang.org/x/sys/unix"

	"flactastic/server/taglib"
)

// maxArtPixels bounds a decoded picture to about 144 MB.
const maxArtPixels = 6000

var errNoArt = errors.New("no artwork")

var thumbMu sync.Mutex

// Picture returns the original embedded picture with this artwork ID.
func (l *Library) Picture(artID string) ([]byte, error) {
	var rel string
	if err := l.db.QueryRow("SELECT path FROM tracks WHERE art = ? AND path IS NOT NULL LIMIT 1", artID).Scan(&rel); err != nil {
		return nil, errNoArt
	}
	r, err := l.Root()
	if err != nil {
		return nil, err
	}
	f, err := r.Open(rel, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dup, err := unix.Dup(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	tf, err := taglib.Open(dup, strings.TrimPrefix(path.Ext(rel), "."), false)
	if err != nil {
		return nil, err
	}
	defer tf.Close()
	if pic := tf.Picture(); pic != nil {
		return pic, nil
	}
	return nil, errNoArt
}

// Thumbnail returns the path of a cached JPEG no larger than size on its
// long edge, making it on first use.
func (l *Library) Thumbnail(artID string, size int) (string, error) {
	file := filepath.Join(l.Dir, "art", artID+"-"+strconv.Itoa(size)+".jpg")
	if _, err := os.Stat(file); err == nil {
		return file, nil
	}
	thumbMu.Lock()
	defer thumbMu.Unlock()
	if _, err := os.Stat(file); err == nil {
		return file, nil
	}
	pic, err := l.Picture(artID)
	if err != nil {
		return "", err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(pic))
	if err != nil || cfg.Width > maxArtPixels || cfg.Height > maxArtPixels || cfg.Width == 0 || cfg.Height == 0 {
		return "", errNoArt
	}
	src, _, err := image.Decode(bytes.NewReader(pic))
	if err != nil {
		return "", err
	}
	w, h := cfg.Width, cfg.Height
	if w > size || h > size {
		if w >= h {
			w, h = size, max(1, h*size/w)
		} else {
			w, h = max(1, w*size/h), size
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	tmp, err := os.CreateTemp(filepath.Dir(file), "thumb")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	err = jpeg.Encode(tmp, dst, &jpeg.Options{Quality: 85})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	return file, err
}
