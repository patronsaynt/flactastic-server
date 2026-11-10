// Package store is server.db: users, device tokens and server settings.
// The CLI and the server share it; the server reads users and tokens per
// request, so a CLI change takes effect without a restart.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/argon2"

	"flactastic/server/meta"
)

// Open opens a SQLite database the way every database here is opened.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_foreign_keys=on&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	return db, db.Ping()
}

// Migrate applies the schema steps a database hasn't seen, in order.
func Migrate(db *sql.DB, steps []string) error {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(steps); i++ {
		if _, err := db.Exec(steps[i] + fmt.Sprintf(";PRAGMA user_version=%d", i+1)); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

var schema = []string{`
CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE users(
	id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, hash TEXT NOT NULL, admin INTEGER NOT NULL,
	root TEXT NOT NULL, library TEXT NOT NULL, quota INTEGER, created TEXT NOT NULL);
CREATE TABLE devices(
	id TEXT PRIMARY KEY, user TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL, kind TEXT NOT NULL, token TEXT NOT NULL UNIQUE, created TEXT NOT NULL, seen TEXT NOT NULL);
CREATE TABLE requests(id INTEGER PRIMARY KEY, kind TEXT NOT NULL, user TEXT NOT NULL)`}

type Store struct {
	DB  *sql.DB
	Dir string
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	db, err := Open(filepath.Join(dir, "server.db"))
	if err != nil {
		return nil, err
	}
	if err := Migrate(db, schema); err != nil {
		return nil, err
	}
	s := &Store{DB: db, Dir: dir}
	_, err = db.Exec("INSERT OR IGNORE INTO settings VALUES('id', ?)", meta.NewUUID().String())
	return s, err
}

func (s *Store) Setting(key string) (v string) {
	s.DB.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return
}

type User struct {
	ID, Name, Root, Library string
	Admin                   bool
	Quota                   *int64
}

var ErrNotFound = errors.New("not found")

var NamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func (s *Store) AddUser(name, password, root, library string, admin bool, quota *int64) (User, error) {
	if !NamePattern.MatchString(name) {
		return User{}, errors.New("usernames are lowercase letters, digits, '-' and '_', starting with a letter")
	}
	if err := CheckPassword(password); err != nil {
		return User{}, err
	}
	u := User{ID: meta.NewUUID().String(), Name: name, Root: root, Library: library, Admin: admin, Quota: quota}
	_, err := s.DB.Exec("INSERT INTO users VALUES(?,?,?,?,?,?,?,?)",
		u.ID, name, HashPassword(password), admin, root, library, quota, now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return User{}, fmt.Errorf("user %q already exists", name)
	}
	return u, err
}

const userCols = "id, name, root, library, admin, quota"

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.Root, &u.Library, &u.Admin, &u.Quota)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return u, err
}

func (s *Store) UserByName(name string) (User, error) {
	return scanUser(s.DB.QueryRow("SELECT "+userCols+" FROM users WHERE name = ?", name))
}

func (s *Store) Users() ([]User, error) {
	rows, err := s.DB.Query("SELECT " + userCols + " FROM users ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUser changes one column of a user; the column name never comes from input.
func (s *Store) SetUser(name, column string, value any) error {
	if column == "hash" {
		value = HashPassword(value.(string))
	}
	r, err := s.DB.Exec("UPDATE users SET "+column+" = ? WHERE name = ?", value, name)
	return affected(r, err)
}

func (s *Store) DeleteUser(name string) error {
	return affected(s.DB.Exec("DELETE FROM users WHERE name = ?", name))
}

func affected(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Request queues work for the running server (a scan or rebuild) from the CLI.
func (s *Store) Request(kind, userID string) error {
	_, err := s.DB.Exec("INSERT INTO requests(kind, user) VALUES(?, ?)", kind, userID)
	return err
}

func (s *Store) TakeRequests() (out [][2]string) {
	rows, err := s.DB.Query("DELETE FROM requests RETURNING kind, user")
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var r [2]string
		if rows.Scan(&r[0], &r[1]) == nil {
			out = append(out, r)
		}
	}
	return out
}

// Passwords

const argonTime, argonMemory, argonThreads, argonLen = 2, 19 * 1024, 1, 32

func CheckPassword(p string) error {
	if len(p) < 8 || len(p) > 1024 {
		return errors.New("passwords are 8 to 1024 bytes")
	}
	return nil
}

func HashPassword(p string) string {
	salt := random(16)
	key := argon2.IDKey([]byte(p), salt, argonTime, argonMemory, argonThreads, argonLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(key))
}

func VerifyPassword(hash, p string) bool {
	var m, t uint32
	var par uint8
	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &par); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(p), salt, t, m, par, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash makes a sign-in for an unknown user cost the same as a real one.
var dummyHash = HashPassword(base64.RawStdEncoding.EncodeToString(random(16)))

// Authenticate checks a password, taking the same time whether or not the
// user exists.
func (s *Store) Authenticate(name, password string) (User, bool) {
	var hash string
	u, err := scanUser(s.DB.QueryRow("SELECT "+userCols+" FROM users WHERE name = ?", name))
	if err == nil {
		err = s.DB.QueryRow("SELECT hash FROM users WHERE id = ?", u.ID).Scan(&hash)
	}
	if err != nil {
		VerifyPassword(dummyHash, password)
		return User{}, false
	}
	return u, VerifyPassword(hash, password)
}

// Devices

type Device struct {
	ID, User, Name, Kind string
	Created, Seen        time.Time
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (s *Store) AddDevice(userID, name, kind string) (Device, string, error) {
	token := base64.RawURLEncoding.EncodeToString(random(32))
	d := Device{ID: meta.NewUUID().String(), User: userID, Name: name, Kind: kind, Created: time.Now().UTC()}
	d.Seen = d.Created
	_, err := s.DB.Exec("INSERT INTO devices VALUES(?,?,?,?,?,?,?)", d.ID, userID, name, kind, tokenHash(token), now(), now())
	return d, token, err
}

// DeviceForToken looks a token up by its SHA-256: an exact index match on a
// value derived from the secret, so timing reveals nothing about the token.
func (s *Store) DeviceForToken(token string) (Device, User, error) {
	var d Device
	var u User
	var created, seen string
	err := s.DB.QueryRow(`SELECT d.id, d.user, d.name, d.kind, d.created, d.seen, u.id, u.name, u.root, u.library, u.admin, u.quota
		FROM devices d JOIN users u ON u.id = d.user WHERE d.token = ?`, tokenHash(token)).
		Scan(&d.ID, &d.User, &d.Name, &d.Kind, &created, &seen, &u.ID, &u.Name, &u.Root, &u.Library, &u.Admin, &u.Quota)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	d.Created, _ = time.Parse(time.RFC3339Nano, created)
	d.Seen, _ = time.Parse(time.RFC3339Nano, seen)
	return d, u, err
}

// Touch records that a device was seen, at most once a minute.
func (s *Store) Touch(d Device) {
	if time.Since(d.Seen) > time.Minute {
		s.DB.Exec("UPDATE devices SET seen = ? WHERE id = ?", now(), d.ID)
	}
}

// Devices lists a user's devices, or every device when userID is empty.
func (s *Store) Devices(userID string) ([]Device, error) {
	rows, err := s.DB.Query("SELECT id, user, name, kind, created, seen FROM devices WHERE ? = '' OR user = ? ORDER BY created", userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		var created, seen string
		if err := rows.Scan(&d.ID, &d.User, &d.Name, &d.Kind, &created, &seen); err != nil {
			return nil, err
		}
		d.Created, _ = time.Parse(time.RFC3339Nano, created)
		d.Seen, _ = time.Parse(time.RFC3339Nano, seen)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokeDevice removes a device; with userID set, only one of that user's.
func (s *Store) RevokeDevice(id, userID string) error {
	return affected(s.DB.Exec("DELETE FROM devices WHERE id = ? AND (? = '' OR user = ?)", id, userID, userID))
}

// RevokeOthers removes every device of a user except keep.
func (s *Store) RevokeOthers(userID, keep string) error {
	_, err := s.DB.Exec("DELETE FROM devices WHERE user = ? AND id != ?", userID, keep)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}
