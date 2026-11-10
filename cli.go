package main

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"golang.org/x/sys/unix"

	"flactastic/server/library"
	"flactastic/server/store"
)

const usage = `FLACtastic Server

Usage:
  flactastic-server serve [--listen :4410]
  flactastic-server info
  flactastic-server user add NAME --root PATH [--admin] [--quota SIZE] [--password-stdin]
  flactastic-server user list
  flactastic-server user passwd NAME [--password-stdin]
  flactastic-server user set NAME [--root PATH] [--quota SIZE|none] [--admin=true|false]
  flactastic-server user del NAME
  flactastic-server device list [NAME]
  flactastic-server device revoke ID
  flactastic-server scan [NAME]
  flactastic-server rebuild [NAME]
  flactastic-server backup DIR
  flactastic-server version

Every command takes --data DIR (default /var/lib/flactastic-server).
SIZE is bytes or a number with K, M, G or T. Removing a user never touches their music.
`

const dropIn = "/etc/systemd/system/flactastic-server.service.d/libraries.conf"

type opts struct {
	data, listen, root, quota, admin string
	passwordStdin                    bool
	args                             []string
}

// parse accepts flags anywhere, so a name can come before them.
func parse(args []string) (opts, error) {
	o := opts{data: "/var/lib/flactastic-server", listen: ":4410"}
	values := map[string]*string{"--data": &o.data, "--listen": &o.listen, "--root": &o.root, "--quota": &o.quota, "--admin": &o.admin}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			o.args = append(o.args, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		switch {
		case name == "--password-stdin" && !hasValue:
			o.passwordStdin = true
		case name == "--admin" && !hasValue:
			o.admin = "true"
		case values[name] != nil:
			if !hasValue {
				if i++; i == len(args) {
					return o, fmt.Errorf("%s needs a value", name)
				}
				value = args[i]
			}
			*values[name] = value
		case name == "--help":
			o.args = []string{"help"}
			return o, nil
		default:
			return o, fmt.Errorf("unknown option %s", a)
		}
	}
	return o, nil
}

func run(args []string) error {
	o, err := parse(args)
	if err != nil {
		return err
	}
	if len(o.args) == 0 || o.args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	switch o.args[0] {
	case "serve":
		return serve(o)
	case "version":
		fmt.Println(version)
		return nil
	}
	s, err := store.New(o.data)
	if err != nil {
		return err
	}
	defer s.DB.Close()
	if os.Geteuid() == 0 {
		defer settle(s)
	}
	cmd := strings.Join(o.args[:min(2, len(o.args))], " ")
	arg := func(i int) string {
		if len(o.args) > i {
			return o.args[i]
		}
		return ""
	}
	switch {
	case cmd == "info":
		return info(o, s)
	case cmd == "user add":
		return userAdd(o, s, arg(2))
	case cmd == "user list":
		return userList(s)
	case cmd == "user passwd":
		return userPasswd(o, s, arg(2))
	case cmd == "user set":
		return userSet(o, s, arg(2))
	case cmd == "user del":
		u, err := s.UserByName(arg(2))
		if err != nil {
			return fmt.Errorf("no user %q", arg(2))
		}
		if err := s.DeleteUser(u.Name); err != nil {
			return err
		}
		return os.RemoveAll(filepath.Join(o.data, "users", u.ID))
	case cmd == "device list":
		return deviceList(s, arg(2))
	case cmd == "device revoke":
		return s.RevokeDevice(arg(2), "")
	case o.args[0] == "scan", o.args[0] == "rebuild":
		users, err := s.Users()
		for _, u := range users {
			if arg(1) == "" || u.Name == arg(1) {
				err = errors.Join(err, s.Request(o.args[0], u.ID))
			}
		}
		return err
	case o.args[0] == "backup" && arg(1) != "":
		return backup(o, s, arg(1))
	}
	return fmt.Errorf("unknown command %q; see --help", strings.Join(o.args, " "))
}

func info(o opts, s *store.Store) error {
	cert, err := certificate(o.data)
	if err != nil {
		return err
	}
	name, _ := os.Hostname()
	_, port, _ := net.SplitHostPort(o.listen)
	fmt.Printf("name: %s\nid: %s\nversion: %s\nport: %s\nfingerprint: %s\n", name, s.Setting("id"), version, port, fingerprint(cert))
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.IsGlobalUnicast() {
			fmt.Printf("address: https://%s\n", net.JoinHostPort(ip.IP.String(), port))
		}
	}
	return nil
}

func checkRoot(s *store.Store, root, self string) (string, error) {
	if !filepath.IsAbs(root) || strings.ContainsAny(root, "\"\\\n") {
		return "", errors.New("--root must be an absolute path without quotes, backslashes or newlines")
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", root)
	}
	users, _ := s.Users()
	for _, u := range users {
		other, err := filepath.EvalSymlinks(u.Root)
		if u.Name == self || err != nil {
			continue
		}
		if within(real, other) || within(other, real) {
			return "", fmt.Errorf("%s overlaps %s's library", root, u.Name)
		}
	}
	return real, nil
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func parseSize(v string) (*int64, error) {
	if v == "" || v == "none" {
		return nil, nil
	}
	mult := int64(1)
	if i := strings.IndexAny(strings.ToUpper(v), "KMGT"); i == len(v)-1 {
		mult = int64(1) << (10 * (strings.IndexByte("KMGT", strings.ToUpper(v)[i]) + 1))
		v = v[:i]
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("bad size %q", v)
	}
	n *= mult
	return &n, nil
}

func readPassword(fromStdin bool) (string, error) {
	if fromStdin || !isTerminal(0) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	ask := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		t, err := unix.IoctlGetTermios(0, unix.TCGETS)
		if err != nil {
			return "", err
		}
		quiet := *t
		quiet.Lflag &^= unix.ECHO
		unix.IoctlSetTermios(0, unix.TCSETS, &quiet)
		defer unix.IoctlSetTermios(0, unix.TCSETS, t)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Fprintln(os.Stderr)
		return strings.TrimRight(line, "\r\n"), err
	}
	p, err := ask("Password: ")
	if err != nil {
		return "", err
	}
	if again, err := ask("Again: "); err != nil || again != p {
		return "", errors.New("passwords don't match")
	}
	return p, nil
}

func userAdd(o opts, s *store.Store, name string) error {
	if o.root == "" {
		return errors.New("user add needs --root PATH")
	}
	root, err := checkRoot(s, o.root, name)
	if err != nil {
		return err
	}
	quota, err := parseSize(o.quota)
	if err != nil {
		return err
	}
	password, err := readPassword(o.passwordStdin)
	if err != nil {
		return err
	}
	id, err := library.EnsureIdentity(root)
	if err != nil {
		return err
	}
	_, err = s.AddUser(name, password, root, id, o.admin == "true", quota)
	return err
}

func userPasswd(o opts, s *store.Store, name string) error {
	u, err := s.UserByName(name)
	if err != nil {
		return fmt.Errorf("no user %q", name)
	}
	p, err := readPassword(o.passwordStdin)
	if err == nil {
		err = store.CheckPassword(p)
	}
	if err != nil {
		return err
	}
	if err := s.SetUser(name, "hash", p); err != nil {
		return err
	}
	return s.RevokeOthers(u.ID, "")
}

func userSet(o opts, s *store.Store, name string) error {
	if _, err := s.UserByName(name); err != nil {
		return fmt.Errorf("no user %q", name)
	}
	if o.root != "" {
		root, err := checkRoot(s, o.root, name)
		if err != nil {
			return err
		}
		id, err := library.EnsureIdentity(root)
		if err != nil {
			return err
		}
		if err := errors.Join(s.SetUser(name, "root", root), s.SetUser(name, "library", id)); err != nil {
			return err
		}
	}
	if o.quota != "" {
		q, err := parseSize(o.quota)
		if err != nil {
			return err
		}
		if err := s.SetUser(name, "quota", q); err != nil {
			return err
		}
	}
	if o.admin != "" {
		return s.SetUser(name, "admin", o.admin == "true")
	}
	return nil
}

func userList(s *store.Store) error {
	users, err := s.Users()
	if err != nil || len(users) == 0 {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tADMIN\tQUOTA\tLIBRARY")
	for _, u := range users {
		quota := "none"
		if u.Quota != nil {
			quota = strconv.FormatInt(*u.Quota, 10)
		}
		fmt.Fprintf(w, "%s\t%v\t%s\t%s\n", u.Name, u.Admin, quota, u.Root)
	}
	return w.Flush()
}

func deviceList(s *store.Store, name string) error {
	users, err := s.Users()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tUSER\tNAME\tKIND\tLAST SEEN")
	for _, u := range users {
		if name != "" && u.Name != name {
			continue
		}
		devices, _ := s.Devices(u.ID)
		for _, d := range devices {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", d.ID, u.Name, d.Name, d.Kind, d.Seen.Local().Format("2006-01-02 15:04"))
		}
	}
	return w.Flush()
}

// backup copies every database with SQLite's VACUUM INTO, which is safe
// while the server runs.
func backup(o opts, s *store.Store, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	into := func(db *sql.DB, file string) error {
		os.Remove(file)
		_, err := db.Exec("VACUUM INTO ?", file)
		return err
	}
	if err := into(s.DB, filepath.Join(dir, "server.db")); err != nil {
		return err
	}
	users, _ := s.Users()
	for _, u := range users {
		db, err := store.Open(filepath.Join(o.data, "users", u.ID, "index.db"))
		if err != nil {
			continue
		}
		os.MkdirAll(filepath.Join(dir, "users", u.ID), 0o700)
		err = into(db, filepath.Join(dir, "users", u.ID, "index.db"))
		db.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// settle runs after any CLI command as root: files root created in the data
// directory or a library's .flactastic folder go back to the service user,
// and the unit's drop-in grants it write access to every library.
func settle(s *store.Store) {
	st, err := os.Stat(s.Dir)
	if err != nil {
		return
	}
	uid, gid := int(st.Sys().(*syscall.Stat_t).Uid), int(st.Sys().(*syscall.Stat_t).Gid)
	if uid == 0 {
		return
	}
	chown := func(dir string) {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				os.Lchown(p, uid, gid)
			}
			return nil
		})
	}
	chown(s.Dir)
	users, _ := s.Users()
	conf := "[Service]\n"
	for _, u := range users {
		chown(filepath.Join(u.Root, ".flactastic"))
		conf += fmt.Sprintf("ReadWritePaths=\"-%s\"\n", u.Root)
	}
	if old, _ := os.ReadFile(dropIn); string(old) == conf {
		return
	}
	if _, err := os.Stat("/etc/systemd/system"); err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(dropIn), 0o755)
	if os.WriteFile(dropIn, []byte(conf), 0o644) == nil {
		exec.Command("systemctl", "daemon-reload").Run()
		exec.Command("systemctl", "try-restart", "flactastic-server").Run()
	}
}
