package library

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/draw"
	"golang.org/x/sys/unix"

	"flactastic/server/taglib"
)

// maxArtPixels bounds the pictures a tag edit may embed.
const maxArtPixels = 6000

// Pictures already in files (uploads, scans) may be larger. A thumbnail
// decodes the whole picture, since Go's decoders can't decode at reduced
// scale, so it is bounded by side and by the memory the decode is estimated
// to need (decodeCost): 768 MiB, which admits a baseline 4:2:0 JPEG at the
// full 12000 px, as well as an 8-bit PNG; a progressive JPEG or a 16-bit PNG
// up to about 10000 px square; and every picture maxArtPixels admits except
// a progressive CMYK JPEG. Thumbnails are made one at a time
// (thumbMu), so this is the most a thumbnail can take beyond the server's
// usual footprint.
const (
	thumbMaxPixels = 12000
	thumbMaxBytes  = 768 << 20
)

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
	cfg, format, err := image.DecodeConfig(bytes.NewReader(pic))
	if err != nil || cfg.Width > thumbMaxPixels || cfg.Height > thumbMaxPixels || cfg.Width == 0 || cfg.Height == 0 {
		return "", errNoArt
	}
	cost := decodeCost(pic, format, cfg)
	if cost > thumbMaxBytes {
		return "", errNoArt
	}
	src, _, err := image.Decode(bytes.NewReader(pic))
	if err != nil {
		return "", err
	}
	if cost > 64<<20 {
		defer debug.FreeOSMemory() // don't keep a big decode's heap
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

// decodeCost estimates the bytes image.Decode allocates for pic: the
// decoded planes, plus for a progressive JPEG the coefficients it keeps
// (4 bytes a sample) and for a CMYK JPEG the converted copy.
func decodeCost(pic []byte, format string, cfg image.Config) int64 {
	px := int64(cfg.Width) * int64(cfg.Height)
	if format == "png" {
		switch cfg.ColorModel {
		case color.GrayModel:
			return px
		case color.Gray16Model:
			return 2 * px
		case color.NRGBAModel, color.RGBAModel:
			return 4 * px
		}
		if _, ok := cfg.ColorModel.(color.Palette); ok {
			return px
		}
		return 8 * px // 16-bit colour
	}
	progressive, sampling, ok := jpegFrame(pic)
	if !ok {
		return 24 * px // the worst a JPEG can be: progressive CMYK
	}
	var hmax, vmax int64 = 1, 1
	for _, hv := range sampling {
		hmax, vmax = max(hmax, int64(hv>>4)), max(vmax, int64(hv&15))
	}
	var samples int64
	for _, hv := range sampling {
		w := (int64(cfg.Width)*int64(hv>>4) + hmax - 1) / hmax
		h := (int64(cfg.Height)*int64(hv&15) + vmax - 1) / vmax
		samples += w * h
	}
	cost := samples
	if progressive {
		cost += 4 * samples
	}
	if len(sampling) == 4 {
		cost += 4 * px
	}
	return cost
}

// jpegFrame reads a JPEG's frame header: whether it is progressive, and
// each component's sampling factors (h<<4 | v).
func jpegFrame(b []byte) (progressive bool, sampling []byte, ok bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return false, nil, false
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return false, nil, false
		}
		m := b[i+1]
		if m == 0xFF { // fill byte
			i++
			continue
		}
		if m == 0x01 || m >= 0xD0 && m <= 0xD7 { // no length
			i += 2
			continue
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return false, nil, false
		}
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			seg := b[i+4 : i+2+n]
			if len(seg) < 6 {
				return false, nil, false
			}
			c := int(seg[5])
			if c < 1 || len(seg) < 6+3*c {
				return false, nil, false
			}
			for k := range c {
				sampling = append(sampling, seg[6+3*k+1])
			}
			return m == 0xC2, sampling, true
		}
		if m == 0xDA { // scan data before any frame header
			return false, nil, false
		}
		i += 2 + n
	}
	return false, nil, false
}
