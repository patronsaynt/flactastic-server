package library

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"regexp"
)

// User data (protocol §14): opaque files that follow a user between devices,
// such as artist profiles and their pictures. The server never reads them.

const (
	MaxUserItem  = 8 << 20
	maxUserTotal = 512 << 20
	maxUserItems = 20000
)

type UserItem struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	ETag string `json:"etag"`
}

// Names are slash-separated segments that can't be "." or "..".
var userName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)

func ValidUserName(name string) bool { return len(name) <= 200 && userName.MatchString(name) }

func (l *Library) UserItems() ([]UserItem, error) {
	rows, err := l.db.Query("SELECT name, size, etag FROM userdata ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserItem{}
	for rows.Next() {
		var i UserItem
		rows.Scan(&i.Name, &i.Size, &i.ETag)
		out = append(out, i)
	}
	return out, rows.Err()
}

func (l *Library) UserItem(name string) (data []byte, etag string, err error) {
	err = l.db.QueryRow("SELECT data, etag FROM userdata WHERE name = ?", name).Scan(&data, &etag)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return
}

// PutUserItem stores data under name and returns its ETag. Storing the same
// bytes again changes nothing, so it wakes no one.
func (l *Library) PutUserItem(name string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	etag := hex.EncodeToString(sum[:16])
	if _, old, err := l.UserItem(name); err == nil && old == etag {
		return etag, nil
	}
	return etag, l.write(func(tx *sql.Tx, _ int64) error {
		var n, total int64
		if err := tx.QueryRow("SELECT COUNT(*), COALESCE(SUM(size), 0) FROM userdata WHERE name != ?", name).Scan(&n, &total); err != nil {
			return err
		}
		if n >= maxUserItems || total+int64(len(data)) > maxUserTotal {
			return ErrNoSpace
		}
		_, err := tx.Exec("INSERT OR REPLACE INTO userdata VALUES(?, ?, ?, ?)", name, etag, len(data), data)
		return err
	}, KindUserData)
}

func (l *Library) DeleteUserItem(name string) error {
	return l.write(func(tx *sql.Tx, _ int64) error {
		r, err := tx.Exec("DELETE FROM userdata WHERE name = ?", name)
		if err == nil {
			if n, _ := r.RowsAffected(); n == 0 {
				err = ErrNotFound
			}
		}
		return err
	}, KindUserData)
}
