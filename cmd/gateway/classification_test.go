package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestNormalizeClassification(t *testing.T) {
	// The AgentRequest CRD requires spec.classification to match this pattern.
	re := regexp.MustCompile(`^[a-z][a-z0-9-]*/[a-z][a-z0-9-]*$`)

	cases := []struct {
		name      string
		in        string
		want      string
		wantValid bool // result is expected to satisfy the CRD pattern
	}{
		{"already valid", "nodepool/at-capacity", "nodepool/at-capacity", true},
		{"spaces", "Nodepool/At Capacity", "nodepool/at-capacity", true},
		{"slash in subcategory (the 400 bug)", "Config/Missing Secret/ConfigMap", "config/missing-secret-configmap", true},
		{"uppercase category", "Config/Bad Config File", "config/bad-config-file", true},
		{"multiple separators collapse", "config//missing  secret", "config/missing-secret", true},
		{"trailing slash (no subcategory)", "config/", "config", false},
		{"empty passes through", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeClassification(c.in)
			if got != c.want {
				t.Fatalf("normalizeClassification(%q) = %q, want %q", c.in, got, c.want)
			}
			if c.wantValid && !re.MatchString(got) {
				t.Errorf("%q -> %q does not satisfy the CRD pattern %s", c.in, got, re.String())
			}
			if c.wantValid && strings.Count(got, "/") != 1 {
				t.Errorf("%q -> %q must contain exactly one slash", c.in, got)
			}
		})
	}
}
