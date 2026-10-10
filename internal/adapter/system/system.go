// Package system is the real clock and identifier source.
package system

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"strings"
	"time"
)

type Clock struct{}

func (Clock) Now() time.Time { return time.Now() }

// IDs makes time-ordered identifiers: prefix_ + 26 lowercase base32
// characters (48-bit milliseconds, 80 random bits). They are valid Docker
// tag and repository path components.
type IDs struct{}

var enc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

func (IDs) New(prefix string) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli())<<16)
	_, _ = rand.Read(b[6:])
	return prefix + "_" + strings.ToLower(enc.EncodeToString(b[:]))
}
