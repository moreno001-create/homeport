// Package auth hashes passwords, makes session tokens, and checks names.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id settings: about 64 MB and a few tens of milliseconds per check on a small server.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// HashPassword returns a self-describing argon2id hash ("argon2id$t$m$p$salt$key").
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("argon2id$%d$%d$%d$%s$%s", argonTime, argonMemory, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// CheckPassword reports whether password matches the hash, in constant time.
func CheckPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "argon2id" {
		return false
	}
	var t, m uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[1]+" "+parts[2]+" "+parts[3], "%d %d %d", &t, &m, &p); err != nil {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[4])
	want, err2 := enc.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NewToken is a random secret for a session or a one-time sign-in code.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source is broken; nothing is safe
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenHash is what's stored for a token, so a leaked state file doesn't hand out sessions.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// NewSecret is a random password for an app's database and the like (letters and digits only, safe in any config).
func NewSecret(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	r := make([]byte, n)
	if _, err := rand.Read(r); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(r[i])%len(alphabet)]
	}
	return string(b)
}

var (
	userRe   = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,31}$`)
	groupRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)
	domainRe = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
)

// ValidUser: 2-32 characters, lowercase, starting with a letter.
func ValidUser(name string) error {
	if !userRe.MatchString(name) {
		return errors.New("user names are 2–32 lowercase letters, digits, dots, dashes or underscores, starting with a letter")
	}
	return nil
}

func ValidGroup(name string) error {
	if !groupRe.MatchString(name) {
		return errors.New("group names are 2–32 lowercase letters, digits, dashes or underscores, starting with a letter")
	}
	return nil
}

func ValidDomain(name string) error {
	if len(name) > 253 || !domainRe.MatchString(name) {
		return fmt.Errorf("%q isn't a domain name like example.com or cloud.example.com", name)
	}
	return nil
}

// ValidPassword asks for a reasonable length; anything else is up to the person.
func ValidPassword(p string) error {
	if len(p) < 10 {
		return errors.New("passwords need at least 10 characters")
	}
	if len(p) > 200 {
		return errors.New("passwords can be at most 200 characters")
	}
	return nil
}
