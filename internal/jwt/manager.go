// Package jwt provides Ed25519 JWT signing and validation for the AIP gateway.
package jwt

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	keyRotationGracePeriod = 5 * time.Minute
	tokenTTL               = 30 * time.Minute
)

// Manager holds an Ed25519 key pair for signing and validating AIP JWTs.
type Manager struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	prevPublic ed25519.PublicKey // previous key, valid for 5-min grace after rotation
	lastRotate time.Time
	clock      func() time.Time
	mu         sync.RWMutex
}

// Claims are the custom JWT claims for AIP tokens.
type Claims struct {
	jwt.RegisteredClaims
	Action   string `json:"action"`
	Resource string `json:"resource"` // fully-qualified target URI (e.g. github://owner/repo, k8s://ns/deployment/name)
	Request  string `json:"request"`
}

// NewManager loads an Ed25519 private key from a PEM file.
// clock is injectable for testing; pass time.Now for production.
func NewManager(keyPath string, clock func() time.Time) (*Manager, error) {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read key file %s: %w", keyPath, err)
	}
	return NewManagerFromPEM(data, clock)
}

// NewManagerFromPEM creates a Manager from raw PEM-encoded key data.
// clock is injectable for testing; pass time.Now for production.
func NewManagerFromPEM(pemData []byte, clock func() time.Time) (*Manager, error) {
	if clock == nil {
		clock = time.Now
	}
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in key data")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	pk, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an Ed25519 key")
	}
	return &Manager{
		privateKey: pk,
		publicKey:  pk.Public().(ed25519.PublicKey),
		lastRotate: clock(),
		clock:      clock,
	}, nil
}

// GenerateEd25519Key generates a new Ed25519 key pair and writes the private
// key to path in PKCS#8 PEM format.
func GenerateEd25519Key(path string) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key for %s: %w", path, err)
	}
	keyData, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal key for %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create key file %s: %w", path, err)
	}
	if err := pem.Encode(file, &pem.Block{Type: "PRIVATE KEY", Bytes: keyData}); err != nil {
		_ = file.Close() // best-effort; the PEM encode error is the primary failure
		return fmt.Errorf("encode PEM for %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close key file %s: %w", path, err)
	}
	return nil
}

// GenerateKeyPair creates a new Ed25519 key pair and returns both the private
// key PEM and the public key PEM.
func GenerateKeyPair() (privatePEM []byte, publicPEM []byte, err error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key: %w", err)
	}

	var privBuf bytes.Buffer
	if err := pem.Encode(&privBuf, &pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}); err != nil {
		return nil, nil, fmt.Errorf("encode private key: %w", err)
	}

	pkix, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		return nil, nil, fmt.Errorf("marshal public key: %w", err)
	}

	var pubBuf bytes.Buffer
	if err := pem.Encode(&pubBuf, &pem.Block{Type: "PUBLIC KEY", Bytes: pkix}); err != nil {
		return nil, nil, fmt.Errorf("encode public key: %w", err)
	}

	return privBuf.Bytes(), pubBuf.Bytes(), nil
}

// ReloadKey re-reads the key file and atomically swaps the in-memory key pair.
// The old public key is kept as prevPublic for a 5-minute grace period so
// tokens signed just before rotation continue to validate.
func (m *Manager) ReloadKey(keyPath string) error {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read key file %s: %w", keyPath, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return fmt.Errorf("no PEM block found in %s", keyPath)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse private key from %s: %w", keyPath, err)
	}
	pk, ok := key.(ed25519.PrivateKey)
	if !ok {
		return fmt.Errorf("not an Ed25519 key in %s", keyPath)
	}

	newPub := pk.Public().(ed25519.PublicKey)

	m.mu.Lock()
	defer m.mu.Unlock()
	if bytes.Equal(newPub, m.publicKey) {
		// Key hasn't changed — no-op, preserve prevPublic and lastRotate
		return nil
	}
	m.prevPublic = m.publicKey
	m.privateKey = pk
	m.publicKey = newPub
	m.lastRotate = m.clock()
	return nil
}

// StartKeyWatcher polls keyPath every interval and reloads the Ed25519 key pair
// when the file changes (e.g. after the controller rotates the Secret). A failed
// reload is passed to logf but does not crash the gateway — the previous key
// remains active so in-flight tokens continue to work.
func (m *Manager) StartKeyWatcher(ctx context.Context, keyPath string, interval time.Duration, logf func(string, ...any)) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := m.ReloadKey(keyPath); err != nil {
					logf("JWT key reload failed (keeping previous key): %v", err)
				}
			}
		}
	}()
}

// MintToken creates a signed JWT for an approved AgentRequest.
// Returns the token string, its expiry time, and any error.
func (m *Manager) MintToken(agentID, action, resource, requestName string) (string, time.Time, error) {
	now := m.clock()
	expiresAt := now.Add(tokenTTL)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "aip-gateway",
			Subject:   agentID,
			ID:        fmt.Sprintf("%s-%d", requestName, now.UnixNano()),
		},
		Action:   action,
		Resource: resource,
		Request:  requestName,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	m.mu.RLock()
	defer m.mu.RUnlock()
	signed, err := token.SignedString(m.privateKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, expiresAt, nil
}

// ValidateToken verifies an AIP JWT and returns its claims.
func (m *Manager) ValidateToken(tokenString string) (*Claims, error) {
	m.mu.RLock()
	pubKey := m.publicKey
	prevPub := m.prevPublic
	lastRotate := m.lastRotate
	m.mu.RUnlock()

	claims, err := m.validateWithKey(tokenString, pubKey)
	if err == nil {
		return claims, nil
	}

	// If current key fails and we're within 5 minutes of rotation,
	// try the previous key to allow in-flight tokens to validate.
	if len(prevPub) > 0 && m.clock().Sub(lastRotate) <= keyRotationGracePeriod {
		claims, prevErr := m.validateWithKey(tokenString, prevPub)
		if prevErr == nil {
			return claims, nil
		}
	}

	return nil, err
}

func (m *Manager) validateWithKey(tokenString string, pubKey ed25519.PublicKey) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodEdDSA {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return pubKey, nil
	},
		jwt.WithTimeFunc(m.clock),
		jwt.WithIssuer("aip-gateway"),
	)
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	return claims, nil
}

// PublicKeyPEM returns the current public key in PEM format.
func (m *Manager) PublicKeyPEM() ([]byte, error) {
	m.mu.RLock()
	pubKey := m.publicKey
	m.mu.RUnlock()

	keyData, err := x509.MarshalPKIXPublicKey(pubKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: "PUBLIC KEY", Bytes: keyData}); err != nil {
		return nil, fmt.Errorf("encode PEM: %w", err)
	}
	return buf.Bytes(), nil
}
