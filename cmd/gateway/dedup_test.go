package main

import (
	"testing"
	"time"

	"github.com/onsi/gomega"
)

func TestDeterministicRequestName(t *testing.T) {
	g := gomega.NewWithT(t)
	dedupWindow := 1 * time.Hour
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	agentIdentity := "agent-test"
	dedupKey := "abc123"

	// Same (dedupKey, window) → same name
	name1 := deterministicRequestName(agentIdentity, dedupKey, dedupWindow, now)
	name2 := deterministicRequestName(agentIdentity, dedupKey, dedupWindow, now)
	g.Expect(name1).To(gomega.Equal(name2))

	// Same dedupKey, different window → different name
	name3 := deterministicRequestName(agentIdentity, dedupKey, dedupWindow, now.Add(dedupWindow))
	g.Expect(name3).NotTo(gomega.Equal(name1))

	// Different dedupKey, same window → different name
	name4 := deterministicRequestName(agentIdentity, "different-key", dedupWindow, now)
	g.Expect(name4).NotTo(gomega.Equal(name1))

	// Name format: <slug>-<8 hex chars>
	g.Expect(name1).To(gomega.MatchRegexp(`^[a-z0-9](?:[a-z0-9-]{0,53}[a-z0-9])?-[0-9a-f]{8}$`))
}

func TestComputeDedupKey(t *testing.T) {
	g := gomega.NewWithT(t)

	// Same inputs produce same key
	k1 := computeDedupKey("agent-1", "restart", "k8s://x", "nodepool/at-capacity", "")
	k2 := computeDedupKey("agent-1", "restart", "k8s://x", "nodepool/at-capacity", "")
	g.Expect(k1).To(gomega.Equal(k2))

	// Different classification produces different key
	k3 := computeDedupKey("agent-1", "restart", "k8s://x", "nodepool/affinity-mismatch", "")
	g.Expect(k3).NotTo(gomega.Equal(k1))

	// Explicit key overrides computed key
	k4 := computeDedupKey("agent-1", "restart", "k8s://x", "anything", "explicit-key")
	g.Expect(k4).To(gomega.Equal("explicit-key"))

	// Explicit key is used regardless of other inputs
	k5 := computeDedupKey("agent-2", "delete", "k8s://y", "other", "explicit-key")
	g.Expect(k5).To(gomega.Equal("explicit-key"))
}
