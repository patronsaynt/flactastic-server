package taglib

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// FuzzRead feeds mutated fixtures to every parser the scanner uses.
func FuzzRead(f *testing.F) {
	exts := []string{"flac", "mp3", "m4a", "wav", "aiff"}
	for i, ext := range exts {
		files, _ := filepath.Glob("../testdata/audio/*." + ext)
		for _, name := range files[:min(2, len(files))] {
			b, _ := os.ReadFile(name)
			f.Add(b, uint8(i))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, kind uint8) {
		fd, err := unix.MemfdCreate("fuzz", 0)
		if err != nil {
			t.Skip()
		}
		unix.Write(fd, data)
		unix.Seek(fd, 0, 0)
		file, err := Open(fd, exts[int(kind)%len(exts)], false)
		if err != nil {
			return
		}
		file.Read()
		file.Picture()
		file.Lyrics()
		file.Close()
	})
}
