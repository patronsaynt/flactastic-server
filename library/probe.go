package library

import (
	"errors"
	"fmt"
	"path"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
)

// ProbeFS runs the syscalls tag edits, uploads and the organizer rely on,
// in root's .flactastic/incoming and in folder beneath root, calling the
// system directly so each line shows what the filesystem itself does. It
// removes everything it makes.
func ProbeFS(root, folder string) ([]string, error) {
	r, err := openRoot(root)
	if err != nil {
		return nil, err
	}
	defer unix.Close(r.fd)
	var out []string
	report := func(step string, err error) bool {
		if err == nil {
			out = append(out, "ok    "+step)
			return true
		}
		var errno unix.Errno
		if errors.As(err, &errno) {
			err = fmt.Errorf("%v (%s)", err, unix.ErrnoName(errno))
		}
		out = append(out, fmt.Sprintf("FAIL  %s: %v", step, err))
		return false
	}
	out = append(out, fmt.Sprintf("info  network filesystem (replaces files the safe way): %v", r.network))
	id := meta.NewUUID().String()[:8]
	in := incomingDir + "probe-" + id + ".m4a"
	if folder == "" || folder == "." {
		folder = ""
	}
	at := func(name string) string { return path.Join(folder, name) }
	a, b, c := at(".flactastic-probe-"+id+"-a.m4a"), at(".flactastic-probe-"+id+"-b.m4a"), at(".flactastic-probe-"+id+"-c.m4a")
	defer func() {
		for _, p := range []string{in, a, b, c} {
			r.Remove(p, false)
		}
	}()

	f, err := r.Create(in)
	if !report("create "+in, err) {
		return out, nil
	}
	_, err = f.Write(make([]byte, 64<<10))
	report("write", err)
	report("fsync", f.Sync())
	report("ftruncate (grow, as an MP4 tag rewrite does)", f.Truncate(96<<10))
	report("ftruncate (shrink)", f.Truncate(32<<10))
	report("fsetxattr "+xattrName, unix.Fsetxattr(int(f.Fd()), xattrName, []byte(id), 0))
	_, err = unix.Fgetxattr(int(f.Fd()), xattrName, make([]byte, 64))
	report("fgetxattr", err)
	f.Close()

	rename := func(from, to string, flags uint) error {
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
		if flags == 0 {
			return unix.Renameat(fd1, b1, fd2, b2)
		}
		return unix.Renameat2(fd1, b1, fd2, b2, flags)
	}
	if !report("rename across folders: incoming -> "+a, rename(in, a, 0)) {
		g, err := r.Create(a)
		if !report("create "+a, err) {
			return out, nil
		}
		g.Close()
	}
	report("renameat2 RENAME_NOREPLACE onto a free name", rename(a, b, unix.RENAME_NOREPLACE))
	g, err := r.Create(a)
	if report("create "+a, err) {
		g.Close()
		err := rename(a, b, unix.RENAME_NOREPLACE)
		switch {
		case errors.Is(err, unix.EEXIST):
			report("renameat2 RENAME_NOREPLACE onto an existing file is refused", nil)
		case err == nil:
			report("renameat2 RENAME_NOREPLACE onto an existing file", errors.New("REPLACED IT: the flag is ignored"))
		default:
			report("renameat2 RENAME_NOREPLACE onto an existing file", err)
		}
	}
	if r.Exists(a) {
		h, _ := r.Open(b, unix.O_RDONLY, 0) // a replace while the target is open, as a stream holds it
		err := rename(a, b, 0)
		if err != nil && !r.Exists(b) {
			err = fmt.Errorf("%w, AND THE TARGET IS GONE: replacing an open file destroys it here", err)
		}
		report("rename over an open file (plain renameat)", err)
		if h != nil {
			h.Close()
		}
	}
	// What the server does instead on a network filesystem.
	for _, p := range []string{a, b} {
		if !r.Exists(p) {
			if g, err := r.Create(p); err == nil {
				g.Close()
			}
		}
	}
	h, _ := r.Open(b, unix.O_RDONLY, 0)
	network := r.network
	r.network = true
	err = r.Replace(a, b)
	r.network = network
	if err == nil && (!r.Exists(b) || r.Exists(a)) {
		err = errors.New("reported success, but the files aren't where they should be")
	}
	report("replace an open file the server's way (move aside, move in, delete)", err)
	if h != nil {
		h.Close()
	}
	fd1, b1, err1 := r.dir(b, false)
	fd2, b2, err2 := r.dir(c, false)
	if err1 == nil && err2 == nil {
		report("hard link", unix.Linkat(fd1, b1, fd2, b2, 0))
		unix.Close(fd1)
		unix.Close(fd2)
	}
	return out, nil
}
