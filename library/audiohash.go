package library

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
)

// span is a byte range [start, end) of a file.
type span struct{ start, end int64 }

// audioSpans finds the bytes of f that hold audio alone, leaving out tags
// and artwork, so a retag leaves their hash alone. The format comes from
// the file's own magic, the extension only telling MP3 (and raw AAC) apart
// from data that is no format at all. ok is false for anything else or a
// file that doesn't parse. Reads only headers: hashFile streams the bytes.
//
//	FLAC         after the last metadata block (and any leading ID3v2)
//	MP3, AAC     without a leading ID3v2, a trailing ID3v1 or an APEv2 tag
//	MP4          the payload of every top-level mdat atom
//	WAV, AIFF    the payload of the data, or SSND, chunk
func audioSpans(f io.ReaderAt, size int64, ext string) (spans []span, ok bool) {
	head := make([]byte, 12)
	if n, _ := f.ReadAt(head, 0); n < 12 {
		return nil, false
	}
	switch {
	case string(head[4:8]) == "ftyp":
		return mp4Spans(f, size)
	case string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return chunkSpans(f, size, 12, "data", binary.LittleEndian)
	case string(head[:4]) == "FORM" && (string(head[8:12]) == "AIFF" || string(head[8:12]) == "AIFC"):
		return chunkSpans(f, size, 12, "SSND", binary.BigEndian)
	}
	start := id3v2Size(head)
	if start > size {
		return nil, false
	}
	marker := make([]byte, 4)
	if n, _ := f.ReadAt(marker, start); n == 4 && string(marker) == "fLaC" {
		return flacSpans(f, size, start+4)
	}
	if e := strings.ToLower(ext); e == "mp3" || e == "aac" {
		end := trailingTags(f, size)
		if end <= start {
			return nil, false
		}
		return []span{{start, end}}, true
	}
	return nil, false
}

// id3v2Size is the length of an ID3v2 tag starting head, footer included,
// or 0 when there is none.
func id3v2Size(head []byte) int64 {
	if len(head) < 10 || string(head[:3]) != "ID3" {
		return 0
	}
	var n int64
	for _, b := range head[6:10] {
		if b&0x80 != 0 {
			return 0 // not syncsafe: not a tag
		}
		n = n<<7 | int64(b)
	}
	n += 10
	if head[5]&0x10 != 0 {
		n += 10 // footer
	}
	return n
}

// flacSpans walks the metadata blocks from at; the audio frames follow the
// block marked last.
func flacSpans(f io.ReaderAt, size, at int64) ([]span, bool) {
	h := make([]byte, 4)
	for {
		if n, _ := f.ReadAt(h, at); n < 4 {
			return nil, false
		}
		at += 4 + (int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3]))
		if at > size {
			return nil, false
		}
		if h[0]&0x80 != 0 {
			return []span{{at, size}}, true
		}
	}
}

// trailingTags is where an MP3's audio ends: before an ID3v1 tag and an
// APEv2 tag, in whichever order they were appended.
func trailingTags(f io.ReaderAt, size int64) int64 {
	end := size
	for {
		tail := make([]byte, 32)
		if end >= 128 {
			v1 := make([]byte, 3)
			if n, _ := f.ReadAt(v1, end-128); n == 3 && string(v1) == "TAG" {
				end -= 128
				continue
			}
		}
		if end < 32 {
			return end
		}
		if n, _ := f.ReadAt(tail, end-32); n < 32 || string(tail[:8]) != "APETAGEX" {
			return end
		}
		n := int64(binary.LittleEndian.Uint32(tail[12:16])) // items and footer
		if n < 32 {
			return end // a footer that can't be: stop rather than loop
		}
		if binary.LittleEndian.Uint32(tail[20:24])&(1<<31) != 0 {
			n += 32 // header
		}
		if n > end {
			return end
		}
		end -= n
	}
}

// mp4Spans collects the payload of each top-level mdat atom.
func mp4Spans(f io.ReaderAt, size int64) ([]span, bool) {
	var spans []span
	h := make([]byte, 16)
	for at := int64(0); at < size; {
		if n, _ := f.ReadAt(h[:8], at); n < 8 {
			return nil, false
		}
		n, hdr := int64(binary.BigEndian.Uint32(h[:4])), int64(8)
		switch n {
		case 0: // to the end of the file
			n = size - at
		case 1: // 64-bit size
			if m, _ := f.ReadAt(h[8:16], at+8); m < 8 {
				return nil, false
			}
			n, hdr = int64(binary.BigEndian.Uint64(h[8:16])), 16
		}
		if n < hdr || at+n > size {
			return nil, false
		}
		if bytes.Equal(h[4:8], []byte("mdat")) && n > hdr {
			spans = append(spans, span{at + hdr, at + n})
		}
		at += n
	}
	return spans, len(spans) > 0
}

// chunkSpans finds the payload of chunk id among RIFF or IFF chunks from at.
func chunkSpans(f io.ReaderAt, size, at int64, id string, order binary.ByteOrder) ([]span, bool) {
	h := make([]byte, 8)
	for at+8 <= size {
		if n, _ := f.ReadAt(h, at); n < 8 {
			return nil, false
		}
		n := int64(order.Uint32(h[4:8]))
		end := min(at+8+n, size) // a streamed WAV may claim more than it holds
		if string(h[:4]) == id {
			return []span{{at + 8, end}}, true
		}
		at = end + n%2 // chunks are padded to even length
	}
	return nil, false
}
