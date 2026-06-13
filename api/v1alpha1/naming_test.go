package v1alpha1

import (
	"strings"
	"testing"
)

func TestRegistrationObjectName_Deterministic(t *testing.T) {
	name1 := RegistrationObjectName("payment-bot")
	name2 := RegistrationObjectName("payment-bot")
	if name1 != name2 {
		t.Errorf("expected deterministic names, got %q vs %q", name1, name2)
	}
}

func TestRegistrationObjectName_MaxLength(t *testing.T) {
	long := "abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz"
	name := RegistrationObjectName(long)
	if len(name) > 63 {
		t.Errorf("name length %d exceeds 63 chars: %q", len(name), name)
	}
}

func TestRegistrationObjectName_DifferentHashes(t *testing.T) {
	dot := RegistrationObjectName("payment.bot")
	dash := RegistrationObjectName("payment-bot")
	if dot == dash {
		t.Errorf("expected different names for 'payment.bot' and 'payment-bot', got both %q", dot)
	}
}

func TestRegistrationObjectName_AllowsEmptyFallback(t *testing.T) {
	// Characters that sanitize to empty string
	special := "@@@"
	name := RegistrationObjectName(special)
	if name == "" {
		t.Error("expected non-empty fallback name")
	}
	if len(name) > 63 {
		t.Errorf("name length %d exceeds 63 chars", len(name))
	}
	if !strings.Contains(name, "reg-") {
		t.Errorf("expected fallback prefix 'reg-' in name %q", name)
	}
}

func TestRegistrationObjectName_DNSValid(t *testing.T) {
	cases := []string{
		"simple-agent",
		"UPPERCASE-AGENT",
		"agent with spaces",
		"agent.under_score",
	}
	for _, tc := range cases {
		name := RegistrationObjectName(tc)
		for _, ch := range name {
			if !isValidDNSChar(ch) {
				t.Errorf("name %q (from %q) contains invalid DNS char %q", name, tc, ch)
			}
		}
		if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
			t.Errorf("name %q (from %q) starts or ends with '-'", name, tc)
		}
	}
}

func isValidDNSChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-'
}
