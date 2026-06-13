package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var invalidDNSChars = regexp.MustCompile(`[^a-z0-9-]`)

// RegistrationObjectName returns the stable K8s metadata.name for an
// AgentRegistration derived from agentIdentity.
// Format: <dns-slug>-<8 hex chars of sha256(agentIdentity)>
// The hash suffix prevents sanitization collisions:
//
//	"payment.bot" and "payment-bot" both sanitize to "payment-bot" but differ in hash.
//
// The hash deliberately excludes the issuer — see EP "Issuer binding".
// Total length bounded to ≤ 63 chars (K8s name limit for most resources).
// Hashing agentIdentity alone is what lets Create enforce
// one-registration-per-flat-name.
func RegistrationObjectName(agentIdentity string) string {
	h := sha256.Sum256([]byte(agentIdentity))
	suffix := hex.EncodeToString(h[:])[:8]
	slug := sanitizeDNSSegment(agentIdentity, 54)
	if slug == "" {
		slug = "reg"
	}
	return slug + "-" + suffix
}

// ProfileNameForAgent returns the deterministic Kubernetes resource name used
// for the AgentTrustProfile and DiagnosticAccuracySummary of a given agent.
// The name is a sanitized DNS label: up to 54 chars of the agentIdentity
// followed by an 8-hex-char SHA-256 suffix to ensure uniqueness.
func ProfileNameForAgent(agentIdentity string) string {
	h := sha256.Sum256([]byte(agentIdentity))
	suffix := fmt.Sprintf("%x", h[:4])
	prefix := sanitizeDNSSegment(agentIdentity, 54)
	if prefix == "" {
		prefix = "agent"
	}
	return prefix + "-" + suffix
}

func sanitizeDNSSegment(s string, maxLen int) string {
	s = strings.ToLower(s)
	s = invalidDNSChars.ReplaceAllString(s, "-")
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	s = strings.Trim(s, "-")
	return s
}
