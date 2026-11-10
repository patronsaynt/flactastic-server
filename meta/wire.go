package meta

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// UUID encodes as Swift's UUID does: uppercase, hyphenated.
type UUID [16]byte

func NewUUID() UUID {
	var u UUID
	rand.Read(u[:])
	u[6] = u[6]&0x0F | 0x40
	u[8] = u[8]&0x3F | 0x80
	return u
}

func (u UUID) String() string {
	h := strings.ToUpper(hex.EncodeToString(u[:]))
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ParseUUID accepts the hyphenated form in any case.
func ParseUUID(s string) (UUID, error) {
	var u UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, fmt.Errorf("invalid UUID %q", s)
	}
	b, err := hex.DecodeString(s[:8] + s[9:13] + s[14:18] + s[19:23] + s[24:])
	if err != nil {
		return u, fmt.Errorf("invalid UUID %q", s)
	}
	copy(u[:], b)
	return u, nil
}

func (u UUID) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

func (u *UUID) UnmarshalText(b []byte) (err error) {
	*u, err = ParseUUID(string(b))
	return err
}

// Time encodes as the Mac's ISO8601DateFormatter with fractional seconds:
// UTC, milliseconds rounded to nearest.
type Time struct{ time.Time }

const wireTimeLayout = "2006-01-02T15:04:05.000Z"

func (t Time) String() string { return t.UTC().Round(time.Millisecond).Format(wireTimeLayout) }

func (t Time) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

func (t *Time) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	t.Time = v
	return err
}
