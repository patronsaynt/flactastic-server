package library

import (
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
)

// emptyGrace keeps a scan from removing a folder the user has only just made.
const emptyGrace = 10 * time.Minute

// isAudio is the scanner's test for a file it would index.
func isAudio(name string) bool {
	_, ok := meta.ClassifyExt(strings.TrimPrefix(path.Ext(name), "."))
	return ok && !strings.HasPrefix(name, ".")
}

func litter(name string) bool {
	n := strings.ToLower(name)
	return n == ".ds_store" || n == "thumbs.db" || n == "desktop.ini" || strings.HasPrefix(n, "._")
}

// tidy prunes the folders tracks were just moved or removed from, when the
// selected profile asks for it. Callers hold l.mu.
func (l *Library) tidy(r *Root, dirs ...string) {
	if l.profile(nil).DeleteEmptyOriginals {
		l.prune(r, dirs, false)
	}
}

// prune removes each of dirs, and then its parents, for as long as the
// folder has no audio anywhere beneath it; leftovers such as cover art go
// with it. With strict (a scan) a folder goes only if it holds nothing but
// OS litter and is older than emptyGrace. The root, dot-folders, and folders
// holding a symlink or a dot-folder always stay. Callers hold l.mu.
func (l *Library) prune(r *Root, dirs []string, strict bool) {
	if !strict { // a scan lists parents first, so a whole empty tree goes at once
		sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") })
	}
	for _, dir := range dirs {
		for ; !strings.HasPrefix(dir, ".") && !strings.Contains(dir, "/."); dir = path.Dir(dir) {
			f, err := r.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
			if err != nil {
				break
			}
			ok := clearDir(f, strict, false) // look at everything before deleting anything
			if _, err := f.Seek(0, 0); err != nil {
				ok = false
			}
			ok = ok && clearDir(f, strict, true)
			f.Close()
			if !ok || r.Remove(dir, true) != nil {
				break
			}
		}
	}
}

// clearDir reports whether prune may remove the directory f, and with remove
// empties it. It stops at anything that must stay, so a file that appears
// between the two passes is never deleted.
func clearDir(f *os.File, strict, remove bool) bool {
	if info, err := f.Stat(); err != nil || (strict && !remove && time.Since(info.ModTime()) < emptyGrace) {
		return false
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name, flags := e.Name(), 0
		switch {
		case e.IsDir() && !strings.HasPrefix(name, "."):
			fd, err := unix.Openat(int(f.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return false
			}
			sub := os.NewFile(uintptr(fd), name)
			ok := clearDir(sub, strict, remove)
			sub.Close()
			if !ok {
				return false
			}
			flags = unix.AT_REMOVEDIR
		case e.Type().IsRegular() && !isAudio(name) && (!strict || litter(name)):
		default: // audio, a symlink, a dot-folder, or a file the user may want
			return false
		}
		if remove && unix.Unlinkat(int(f.Fd()), name, flags) != nil {
			return false
		}
	}
	return true
}
