//go:build e2e

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// oidcTestServer is a minimal OIDC provider backed by an RSA keypair.
// Serves /.well-known/openid-configuration and /keys (JWKS).
// Call newOIDCTestServer() to create, and server.Close() to tear down.
type oidcTestServer struct {
	IssuerURL string // base URL of the httptest.Server, e.g. "http://127.0.0.1:PORT"
	server    *httptest.Server
	key       *rsa.PrivateKey
	kid       string
}

func newOIDCTestServer() *oidcTestServer {
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	kid := "test-key-id-1"

	s := &oidcTestServer{
		key: pk,
		kid: kid,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":   s.IssuerURL,
			"jwks_uri": s.IssuerURL + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		jwks := jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{
				{
					Key:       pk.Public(),
					KeyID:     kid,
					Algorithm: string(jose.RS256),
					Use:       "sig",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jwks)
	})

	s.server = httptest.NewServer(mux)
	s.IssuerURL = s.server.URL
	return s
}

// mintToken returns a signed JWT with the given sub, aud, and expiry offset from now.
// Use a negative duration for an already-expired token.
func (s *oidcTestServer) mintToken(sub, aud string, ttl time.Duration) string {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", s.kid),
	)
	if err != nil {
		panic(err)
	}

	now := time.Now()
	claims := jwt.Claims{
		Subject:  sub,
		Audience: jwt.Audience{aud},
		Issuer:   s.IssuerURL,
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(ttl)),
	}

	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		panic(err)
	}

	return raw
}

func (s *oidcTestServer) Close() {
	if s.server != nil {
		s.server.Close()
	}
}
