package jwt

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestPrivateKeyFile(t *testing.T, keyPath string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyData, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	f, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: keyData}); err != nil {
		t.Fatalf("encode PEM: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
}

func TestManager(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "key.pem")
	writeTestPrivateKeyFile(t, keyPath)

	fixedTime := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	mgr, err := NewManager(keyPath, func() time.Time { return fixedTime })
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	token, expiry, err := mgr.MintToken("agent-123", "pull-request", "owner/repo", "req-001")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if token == "" {
		t.Error("expected non-empty token")
	}
	wantExpiry := fixedTime.Add(30 * time.Minute)
	if !expiry.Equal(wantExpiry) {
		t.Errorf("expiry = %v, want %v", expiry, wantExpiry)
	}

	claims, err := mgr.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.Subject != "agent-123" {
		t.Errorf("subject = %q, want %q", claims.Subject, "agent-123")
	}
	if claims.Action != "pull-request" {
		t.Errorf("action = %q, want %q", claims.Action, "pull-request")
	}
	if claims.Resource != "owner/repo" {
		t.Errorf("resource = %q, want %q", claims.Resource, "owner/repo")
	}
	if claims.Request != "req-001" {
		t.Errorf("request = %q, want %q", claims.Request, "req-001")
	}

	_, err = mgr.ValidateToken("invalid-token")
	if err == nil {
		t.Error("expected error for invalid token")
	}

	publicPEM, err := mgr.PublicKeyPEM()
	if err != nil {
		t.Fatalf("PublicKeyPEM: %v", err)
	}
	if len(publicPEM) == 0 {
		t.Error("expected non-empty public key PEM")
	}
}

func TestGenerateEd25519Key(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "test-key.pem")

	if err := GenerateEd25519Key(keyPath); err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}

	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	_, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Error("key is not Ed25519")
	}
}

func TestGenerateKeyPair(t *testing.T) {
	privPEM, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if len(privPEM) == 0 {
		t.Error("expected non-empty private key PEM")
	}
	if len(pubPEM) == 0 {
		t.Error("expected non-empty public key PEM")
	}

	// Verify private key parses
	block, _ := pem.Decode(privPEM)
	if block == nil {
		t.Fatal("no private key PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	_, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Error("private key is not Ed25519")
	}

	// Verify public key parses
	block, _ = pem.Decode(pubPEM)
	if block == nil {
		t.Fatal("no public key PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	_, ok = pub.(ed25519.PublicKey)
	if !ok {
		t.Error("public key is not Ed25519")
	}
}

func TestExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "key.pem")
	writeTestPrivateKeyFile(t, keyPath)

	pastTime := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	mgr, err := NewManager(keyPath, func() time.Time { return pastTime })
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	token, _, err := mgr.MintToken("agent-123", "pull-request", "owner/repo", "req-001")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}

	laterTime := time.Date(2020, 1, 1, 12, 35, 0, 0, time.UTC)
	expiredMgr, err := NewManager(keyPath, func() time.Time { return laterTime })
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_, err = expiredMgr.ValidateToken(token)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestKeyReload(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "key.pem")
	writeTestPrivateKeyFile(t, keyPath)

	now := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	mgr, err := NewManager(keyPath, clock)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	token, _, err := mgr.MintToken("agent-1", "action", "repo", "req-1")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}

	_, err = mgr.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken before reload: %v", err)
	}

	if err := GenerateEd25519Key(keyPath); err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}
	if err := mgr.ReloadKey(keyPath); err != nil {
		t.Fatalf("ReloadKey: %v", err)
	}

	token2, _, err := mgr.MintToken("agent-2", "action", "repo", "req-2")
	if err != nil {
		t.Fatalf("MintToken after reload: %v", err)
	}
	_, err = mgr.ValidateToken(token2)
	if err != nil {
		t.Fatalf("ValidateToken new token after reload: %v", err)
	}

	now = now.Add(6 * time.Minute)
	_, err = mgr.ValidateToken(token)
	if err == nil {
		t.Error("expected error for token signed with old key after grace period expired")
	}
}

func TestKeyReloadGracePeriod(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "key.pem")
	writeTestPrivateKeyFile(t, keyPath)

	startTime := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return startTime }

	mgr, err := NewManager(keyPath, clock)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	token, _, err := mgr.MintToken("agent-1", "action", "repo", "req-1")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}

	if err := GenerateEd25519Key(keyPath); err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}
	if err := mgr.ReloadKey(keyPath); err != nil {
		t.Fatalf("ReloadKey: %v", err)
	}

	startTime = startTime.Add(3 * time.Minute)
	_, err = mgr.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken within grace period: %v", err)
	}

	startTime = startTime.Add(3 * time.Minute)
	_, err = mgr.ValidateToken(token)
	if err == nil {
		t.Error("expected error for token signed with old key after grace period expired")
	}
}
