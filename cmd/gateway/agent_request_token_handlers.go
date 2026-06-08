package main

import (
	"encoding/json"
	"log"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

type tokenResponse struct {
	Token     string `json:"token"`
	Service   string `json:"service"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

func (s *Server) handleGetAgentRequestToken(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())

	// 1. Role check: requireRole
	if !requireRole(s.roles, roleAgent, sub, groups, w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	var body struct {
		Service string `json:"service"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Service == "" {
		writeError(w, http.StatusBadRequest, "service is required")
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	name := r.PathValue("name")

	// 2. Read the AgentRequest using apiReader (direct read, bypass cache)
	var agentReq v1alpha1.AgentRequest
	if err := s.apiReader.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &agentReq); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 3. Ownership check: agentRequest.Spec.AgentIdentity != sub → 403
	if agentReq.Spec.AgentIdentity != sub {
		writeError(w, http.StatusForbidden, "caller does not own this AgentRequest")
		return
	}

	// 4. State check: agentRequest.Status.Phase != PhaseApproved → 409
	if agentReq.Status.Phase != v1alpha1.PhaseApproved {
		writeError(w, http.StatusConflict, "AgentRequest is not in Approved state")
		return
	}

	// 5. Registration lookup: s.regCache.get(agentRequest.Spec.AgentIdentity) → if nil → 404
	if s.regCache == nil || s.regCache.get(agentReq.Spec.AgentIdentity) == nil {
		writeError(w, http.StatusNotFound, "no AgentRegistration found for agent")
		return
	}

	// 6. Provider lookup: s.regCache.providerFor(agentRequest.Spec.AgentIdentity, body.Service) → if nil → 404
	provider := s.regCache.providerFor(agentReq.Spec.AgentIdentity, body.Service)
	if provider == nil {
		writeError(w, http.StatusNotFound, "no credential binding for service")
		return
	}

	// 7. Token resolution
	rawToken := rawOIDCTokenFromCtx(r.Context())
	bearerToken, err := provider.Token(r.Context(), rawToken)
	if err != nil {
		log.Printf("Credential resolution failed for agent=%q service=%q: %v", agentReq.Spec.AgentIdentity, body.Service, err)
		writeError(w, http.StatusInternalServerError, "credential resolution failed")
		return
	}

	// 8. Audit log: log.Printf
	log.Printf("Token issued: agentRequestName=%q service=%q agentIdentity=%q", name, body.Service, sub)

	// TODO: Add expiresAt when credential.Provider is extended to return (token, expiry, error).
	// Currently the Provider interface does not surface expiry, and parsing unverified JWT
	// signatures is insecure and wrong for non-JWT credentials (e.g. static PATs).
	resp := tokenResponse{
		Token:   bearerToken,
		Service: body.Service,
	}

	writeJSON(w, http.StatusOK, resp)
}
