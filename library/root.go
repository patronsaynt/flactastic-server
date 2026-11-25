package library

import (
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
)

// Root confines every filesystem operation to one directory. Each path is
// walked one component at a time with O_NOFOLLOW, every step relative to
// the directory before it, so no symlink, ".." or race can leave the
// library. (openat2's RESOLVE_BENEATH would do the same, but systemd's
// RestrictSUIDSGID= and many container runtimes refuse openat2 outright.)
type Root struct {
	fd      int
	Path    string
	network bool // SMB, NFS and the like: see Replace
}

func openRoot(dir string) (*Root, error) {
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	r := &Root{fd: fd, Path: dir}
	var st unix.Statfs_t
	r.network = unix.Fstatfs(fd, &st) == nil && networkFS[int64(st.Type)]
	runtime.SetFinalizer(r, func(r *Root) { unix.Close(r.fd) })
	return r, nil
}

// ErrEscape is a path that would resolve outside the library.
var ErrEscape = errors.New("path leaves the library")

func (r *Root) openat(dirfd int, rel string, flags int, mode uint32) (int, error) {
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.Contains(p, "\x00") {
			return -1, ErrEscape
		}
	}
	fd := dirfd
	for i, p := range parts {
		f := unix.O_PATH | unix.O_DIRECTORY
		if i == len(parts)-1 {
			f = flags
		}
		next, err := unix.Openat(fd, p, f|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
		if fd != dirfd {
			unix.Close(fd)
		}
		if err == unix.ELOOP || (err == unix.ENOTDIR && i < len(parts)-1) {
			err = ErrEscape
		}
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// Open opens rel beneath the root.
func (r *Root) Open(rel string, flags int, mode uint32) (*os.File, error) {
	fd, err := r.openat(r.fd, rel, flags, mode)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: rel, Err: err}
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// dir opens the directory holding rel, creating missing parents when asked,
// and returns it with rel's last component.
func (r *Root) dir(rel string, create bool) (int, string, error) {
	parent, base := path.Split(rel)
	if base == "" || base == "." || base == ".." {
		return -1, "", ErrEscape
	}
	parent = strings.TrimSuffix(parent, "/")
	if parent == "" {
		fd, err := unix.Dup(r.fd)
		return fd, base, err
	}
	if create {
		cur := ""
		for _, c := range strings.Split(parent, "/") {
			cur = path.Join(cur, c)
			fd, err := r.openat(r.fd, cur, unix.O_PATH|unix.O_DIRECTORY, 0)
			if err == nil {
				unix.Close(fd)
				continue
			}
			pfd, name, err := r.dir(cur, false)
			if err != nil {
				return -1, "", err
			}
			err = unix.Mkdirat(pfd, name, 0o755)
			unix.Close(pfd)
			if err != nil && err != unix.EEXIST {
				return -1, "", err
			}
		}
	}
	fd, err := r.openat(r.fd, parent, unix.O_PATH|unix.O_DIRECTORY, 0)
	return fd, base, err
}

// Rename moves from to to within the root. With noReplace it fails rather
// than overwrite an existing file.
func (r *Root) Rename(from, to string, noReplace bool) error {
	fd1, b1, err := r.dir(from, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd1)
	fd2, b2, err := r.dir(to, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd2)
	if !noReplace {
		return unix.Renameat(fd1, b1, fd2, b2)
	}
	err = renameat2(fd1, b1, fd2, b2, unix.RENAME_NOREPLACE)
	if err == unix.EINVAL || err == unix.ENOSYS || err == unix.EOPNOTSUPP {
		// Many network and FUSE filesystems can't refuse to replace: check
		// first, then rename plainly.
		var st unix.Stat_t
		if unix.Fstatat(fd2, b2, &st, unix.AT_SYMLINK_NOFOLLOW) == nil {
			return unix.EEXIST
		}
		return unix.Renameat(fd1, b1, fd2, b2)
	}
	return err
}

// Replace puts from in place of to, which may exist. On a local disk that is
// one atomic rename. A network filesystem may refuse to replace a file that
// is open (a stream, the hasher, another computer), and SMB then deletes the
// target anyway; so there the original is moved aside first, which open
// files allow, and deleted once the new file is in place. Readers keep the
// old bytes until they close.
func (r *Root) Replace(from, to string) error {
	if !r.network {
		return r.Rename(from, to, false)
	}
	aside := path.Join(path.Dir(to), ".flactastic-old-"+meta.NewUUID().String()+path.Ext(to))
	if err := r.Rename(to, aside, true); err != nil {
		if errors.Is(err, unix.ENOENT) && !r.Exists(to) {
			return r.Rename(from, to, true) // nothing to replace
		}
		return err
	}
	if err := r.Rename(from, to, true); err != nil {
		if back := r.Rename(aside, to, true); back != nil {
			return fmt.Errorf("%w; and the original is left at %s: %v", err, aside, back)
		}
		return err
	}
	r.Remove(aside, false)
	return nil
}

// renameat2 is unix.Renameat2; tests stand in a filesystem without flags.
var renameat2 = unix.Renameat2

func (r *Root) Remove(rel string, isDir bool) error {
	fd, base, err := r.dir(rel, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	flags := 0
	if isDir {
		flags = unix.AT_REMOVEDIR
	}
	return unix.Unlinkat(fd, base, flags)
}

// Exists reports whether rel names anything, without following a final symlink.
func (r *Root) Exists(rel string) bool {
	_, err := r.Stat(rel)
	return err == nil
}

// Stat is lstat beneath the root.
func (r *Root) Stat(rel string) (unix.Stat_t, error) {
	var st unix.Stat_t
	fd, base, err := r.dir(rel, false)
	if err != nil {
		return st, err
	}
	defer unix.Close(fd)
	return st, unix.Fstatat(fd, base, &st, unix.AT_SYMLINK_NOFOLLOW)
}

// Create makes a new file beneath the root, creating its parents.
func (r *Root) Create(rel string) (*os.File, error) {
	fd, base, err := r.dir(rel, true)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	nfd, err := r.openat(fd, base, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0o644)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: rel, Err: err}
	}
	return os.NewFile(uintptr(nfd), rel), nil
}
