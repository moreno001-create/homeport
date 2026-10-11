package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Signer signs Homeport's OpenID Connect tokens (RS256, which every OIDC client supports).
type Signer struct {
	key *rsa.PrivateKey
	kid string
}

// LoadSigner reads the signing key, or makes one the first time (kept root-only next to the state).
func LoadSigner(path string) (*Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, errors.New("the OIDC signing key file isn't PEM")
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("the OIDC signing key isn't RSA")
		}
		return newSigner(rk), nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return newSigner(k), nil
}

// NewTestSigner makes a throwaway key (tests).
func NewTestSigner() *Signer {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	return newSigner(k)
}

func newSigner(k *rsa.PrivateKey) *Signer {
	h := sha256.Sum256(k.PublicKey.N.Bytes())
	return &Signer{key: k, kid: b64(h[:8])}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Sign makes a compact JWT.
func (s *Signer) Sign(claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.kid})
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// Verify checks a JWT Homeport signed and returns its claims, if it hasn't expired.
func (s *Signer) Verify(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	var header struct{ Alg, Kid string }
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	sig, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	pb, err3 := base64.RawURLEncoding.DecodeString(parts[1])
	if err1 != nil || err2 != nil || err3 != nil || json.Unmarshal(hb, &header) != nil || header.Alg != "RS256" || header.Kid != s.kid {
		return nil, errors.New("not a token from this server")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&s.key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return nil, errors.New("bad signature")
	}
	var claims map[string]any
	if json.Unmarshal(pb, &claims) != nil {
		return nil, errors.New("bad claims")
	}
	exp, _ := claims["exp"].(float64)
	if int64(exp) <= time.Now().Unix() {
		return nil, errors.New("expired")
	}
	return claims, nil
}

// JWKS is the public key, for apps to check tokens.
func (s *Signer) JWKS() map[string]any {
	pub := s.key.PublicKey
	return map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": s.kid,
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}}
}
