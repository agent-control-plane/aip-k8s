package v1alpha1

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

var invalidDNSChars = regexp.MustCompile(`[^a-z0-9-]`)

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
