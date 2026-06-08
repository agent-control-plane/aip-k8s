package main

import (
	"encoding/json"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

// handleCreateAgentRegistration creates a new AgentRegistration.
func (s *Server) handleCreateAgentRegistration(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var reg v1alpha1.AgentRegistration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if reg.Spec.AgentIdentity == "" {
		writeError(w, http.StatusBadRequest, "agentIdentity is required")
		return
	}

	reg.Namespace = ns

	if err := s.client.Create(r.Context(), &reg); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, reg)
}

// handleListAgentRegistrations lists AgentRegistrations.
func (s *Server) handleListAgentRegistrations(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	if !requireAnyRole(s.roles, []string{roleAdmin, roleReviewer}, sub, groups, w) {
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var list v1alpha1.AgentRegistrationList
	if err := s.client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, list)
}

// handleGetAgentRegistration gets a single AgentRegistration by name.
func (s *Server) handleGetAgentRegistration(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	if !requireAnyRole(s.roles, []string{roleAdmin, roleReviewer}, sub, groups, w) {
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	name := r.PathValue("name")

	var reg v1alpha1.AgentRegistration
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &reg); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, reg)
}

// handleReplaceAgentRegistration updates/replaces an existing AgentRegistration.
func (s *Server) handleReplaceAgentRegistration(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	name := r.PathValue("name")

	var newReg v1alpha1.AgentRegistration
	if err := json.NewDecoder(r.Body).Decode(&newReg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if newReg.Name != "" && newReg.Name != name {
		writeError(w, http.StatusBadRequest, "name in body must match name in path")
		return
	}

	if newReg.Spec.AgentIdentity == "" {
		writeError(w, http.StatusBadRequest, "agentIdentity is required")
		return
	}

	var updated v1alpha1.AgentRegistration
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &updated); err != nil {
			return err
		}
		updated.Spec = newReg.Spec
		updated.Labels = newReg.Labels
		updated.Annotations = newReg.Annotations
		return s.client.Update(r.Context(), &updated)
	})

	if err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteAgentRegistration deletes an AgentRegistration.
func (s *Server) handleDeleteAgentRegistration(w http.ResponseWriter, r *http.Request) {
	groups := callerGroupsFromCtx(r.Context())
	sub := callerSubFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	name := r.PathValue("name")
	namespaceQuery := r.URL.Query().Get("namespace")
	if namespaceQuery == "" {
		namespaceQuery = defaultNamespace
	}

	regToDelete := &v1alpha1.AgentRegistration{}
	regToDelete.Name = name
	regToDelete.Namespace = namespaceQuery

	delErr := s.client.Delete(r.Context(), regToDelete)
	if delErr != nil {
		if apierrors.IsNotFound(delErr) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, delErr.Error())
		return
	}

	var checkedReg v1alpha1.AgentRegistration
	key := types.NamespacedName{Namespace: namespaceQuery, Name: name}
	getErr := s.apiReader.Get(r.Context(), key, &checkedReg)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, getErr.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
