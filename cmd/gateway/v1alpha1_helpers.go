package main

import (
	"encoding/json"
	"net/http"

	v1alpha1openapi "github.com/agent-control-plane/aip-k8s/internal/openapi/v1alpha1"
)

// writeProblem writes an RFC 7807 application/problem+json response.
// Used by all v1alpha1 handlers; legacy handlers continue to use writeError.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	title := http.StatusText(status)
	typ := "about:blank"
	prob := v1alpha1openapi.Problem{
		Type:   &typ,
		Title:  &title,
		Status: &status,
		Detail: &detail,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(prob)
}

// requireRoleV1 is requireRole adapted for v1alpha1 handlers: uses writeProblem
// for 403 responses so the content type matches the OpenAPI spec.
func requireRoleV1(rc *roleConfig, role, sub string, groups []string, w http.ResponseWriter) bool {
	switch role {
	case roleAgent:
		if !rc.isAgent(sub, groups) {
			writeProblem(w, http.StatusForbidden, "agent role required")
			return false
		}
	case roleReviewer:
		if !rc.isReviewer(sub, groups) {
			writeProblem(w, http.StatusForbidden, "reviewer role required")
			return false
		}
	case roleAdmin:
		if !rc.isAdmin(sub, groups) {
			writeProblem(w, http.StatusForbidden, "admin role required")
			return false
		}
	default:
		writeProblem(w, http.StatusForbidden, "unknown role")
		return false
	}
	return true
}
