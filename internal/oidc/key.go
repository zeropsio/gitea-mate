package oidc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
)

// Key is the broker's ES256 signing key, derived from OIDC_SEED so that a
// restart — or a rebuilt container — signs with the same key without ever
// storing one.
type Key struct {
	private *ecdsa.PrivateKey
	kid     string
}

// DeriveKey turns the seed into a P-256 key: SHA-256 of the seed is the
// scalar, reduced into the curve's order, and the public point follows from it.
//
// The kid is the first eight hex characters of the SHA-256 of the *public*
// point. docs/broker-api.md says "the ES256 key derived from OIDC_SEED (kid =
// the first 8 hex of its SHA-256)"; taking "its" as the seed's digest would
// publish the first 32 bits of the private scalar in every JWKS response, so
// the key's own digest is the reading this implements.
func DeriveKey(seed string) (*Key, error) {
	if seed == "" {
		return nil, fmt.Errorf("oidc: the signing seed is empty")
	}
	sum := sha256.Sum256([]byte(seed))

	curve := elliptic.P256()
	n := curve.Params().N
	d := new(big.Int).SetBytes(sum[:])
	d.Mod(d, n)
	if d.Sign() == 0 {
		// Unreachable for any real seed; a zero scalar has no public point.
		d.SetInt64(1)
	}

	x, y := curve.ScalarBaseMult(d.Bytes())
	priv := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d}

	point := elliptic.Marshal(curve, x, y) //nolint:staticcheck // the uncompressed point is what the kid hashes
	fingerprint := sha256.Sum256(point)

	return &Key{private: priv, kid: hex.EncodeToString(fingerprint[:])[:8]}, nil
}

// KeyID is the kid a JWKS entry and every id_token header carry.
func (k *Key) KeyID() string { return k.kid }

// JWKS is the document GET /oidc/jwks serves.
func (k *Key) JWKS() map[string]any {
	size := (k.private.Curve.Params().BitSize + 7) / 8
	return map[string]any{"keys": []any{map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"use": "sig",
		"alg": "ES256",
		"kid": k.kid,
		"x":   b64(k.private.X.FillBytes(make([]byte, size))),
		"y":   b64(k.private.Y.FillBytes(make([]byte, size))),
	}}}
}

// Sign makes a compact ES256 JWT of the claims.
func (k *Key) Sign(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": k.kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64(header) + "." + b64(payload)

	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, k.private, digest[:])
	if err != nil {
		return "", err
	}
	// JWS fixes each half at the curve's byte size; DER would not verify.
	size := (k.private.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, 2*size)
	r.FillBytes(sig[:size])
	s.FillBytes(sig[size:])

	return signingInput + "." + b64(sig), nil
}

func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
