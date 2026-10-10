package domain

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"strings"
	"time"
)

var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// NewID returns a prefixed, roughly time-sortable, collision-resistant identifier,
// e.g. "job_01hx3k...". 48 bits of millisecond time + 80 bits of randomness.
func NewID(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint64(b[0:8], ms<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return prefix + "_" + strings.ToLower(idEncoding.EncodeToString(b[:]))
}

// ID prefixes.
const (
	PrefixTenant      = "ten"
	PrefixUser        = "usr"
	PrefixWorkspace   = "ws"
	PrefixRepository  = "repo"
	PrefixAgent       = "agt"
	PrefixFramework   = "fw"
	PrefixCommand     = "cmd"
	PrefixJob         = "job"
	PrefixExecution   = "exe"
	PrefixInteraction = "int"
	PrefixResponse    = "rsp"
	PrefixArtifact    = "art"
)
