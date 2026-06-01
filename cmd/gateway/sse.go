package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

const (
	sseEventUpdate = "update"
	sseEventResult = "result"
	sseEventError  = "error"
)

func acceptsSSE(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept"), ",") {
		mediaType, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.TrimSpace(mediaType) == "text/event-stream" {
			return true
		}
	}
	return false
}

func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

func writeSSEEvent(w http.ResponseWriter, rc *http.ResponseController, eventType string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshalling SSE data: %w", err)
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, raw)
	if err != nil {
		return fmt.Errorf("writing SSE event: %w", err)
	}
	return rc.Flush()
}

func writeSSEError(w http.ResponseWriter, rc *http.ResponseController, msg string) {
	if err := writeSSEEvent(w, rc, sseEventError, map[string]string{"error": msg}); err != nil {
		log.Printf("SSE: failed to write error event: %v", err)
	}
}

func agentRequestPayload(ar *v1alpha1.AgentRequest, labels map[string]string) map[string]any {
	return map[string]any{
		"name":                     ar.Name,
		"labels":                   labels,
		"phase":                    ar.Status.Phase,
		"denial":                   ar.Status.Denial,
		"conditions":               ar.Status.Conditions,
		"controlPlaneVerification": ar.Status.ControlPlaneVerification,
	}
}

func isTerminalOrActionable(ar *v1alpha1.AgentRequest) bool {
	phase := ar.Status.Phase
	if phase == v1alpha1.PhaseApproved || phase == v1alpha1.PhaseDenied ||
		phase == v1alpha1.PhaseCompleted || phase == v1alpha1.PhaseFailed ||
		phase == v1alpha1.PhaseExpired || phase == v1alpha1.PhaseAwaitingVerdict {
		return true
	}
	if phase == v1alpha1.PhasePending &&
		meta.IsStatusConditionTrue(ar.Status.Conditions, v1alpha1.ConditionRequiresApproval) {
		return true
	}
	return false
}

func (s *Server) streamAgentRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
	name, ns string,
	reqLabels map[string]string,
) {
	rc := http.NewResponseController(w)

	writeSSEHeaders(w)
	if err := rc.Flush(); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.waitTimeout)
	defer cancel()

	// Get-then-watch: read current state and capture resourceVersion, then
	// start the watch from that point. This closes the race between Create()
	// returning and the watch being established — the watch picks up exactly
	// where the Get left off with no gap and no duplicates.
	var current v1alpha1.AgentRequest
	if err := s.apiReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &current); err != nil {
		writeSSEError(w, rc, fmt.Sprintf("failed to get AgentRequest: %v", err))
		return
	}

	if isTerminalOrActionable(&current) {
		if err := writeSSEEvent(w, rc, sseEventResult, agentRequestPayload(&current, reqLabels)); err != nil {
			log.Printf("SSE: failed to write terminal result for %s: %v", name, err)
		}
		return
	}
	if current.Status.Phase != "" {
		if err := writeSSEEvent(w, rc, sseEventUpdate, agentRequestPayload(&current, reqLabels)); err != nil {
			return
		}
	}

	// Raw carries ResourceVersion; controller-runtime merges InNamespace/MatchingFields into it.
	watcher, err := s.watchClient.Watch(ctx, &v1alpha1.AgentRequestList{},
		client.InNamespace(ns),
		client.MatchingFields{"metadata.name": name},
		&client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: current.ResourceVersion}},
	)
	if err != nil {
		writeSSEError(w, rc, fmt.Sprintf("failed to watch AgentRequest: %v", err))
		return
	}
	defer watcher.Stop()

	var lastStatusJSON string
	for {
		select {
		case <-ctx.Done():
			if r.Context().Err() == nil {
				writeSSEError(w, rc, "timed out waiting for AgentRequest resolution")
			}
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				writeSSEError(w, rc, "watch channel closed unexpectedly")
				return
			}
			if event.Type == watch.Error {
				writeSSEError(w, rc, "watch error from API server")
				return
			}
			if event.Type == watch.Deleted {
				writeSSEError(w, rc, "AgentRequest was deleted")
				return
			}
			if event.Type != watch.Added && event.Type != watch.Modified {
				continue
			}

			ar, ok := event.Object.(*v1alpha1.AgentRequest)
			if !ok {
				continue
			}

			if ar.Name != name {
				continue
			}

			if isTerminalOrActionable(ar) {
				if err := writeSSEEvent(w, rc, sseEventResult, agentRequestPayload(ar, reqLabels)); err != nil {
					log.Printf("SSE: failed to write terminal result for %s: %v", name, err)
				}
				return
			}

			statusJSON, err := json.Marshal(ar.Status)
			if err != nil {
				log.Printf("SSE: failed to marshal status for %s: %v", name, err)
			} else if string(statusJSON) == lastStatusJSON {
				continue
			} else {
				lastStatusJSON = string(statusJSON)
			}

			if err := writeSSEEvent(w, rc, sseEventUpdate, agentRequestPayload(ar, reqLabels)); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleWatchAgentRequest(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	if !acceptsSSE(r) {
		writeError(w, http.StatusBadRequest, "Accept header must include text/event-stream")
		return
	}

	var ar v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ar); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("AgentRequest %s not found", name))
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get AgentRequest %s: %v", name, err))
		}
		return
	}

	reqLabels := ar.Labels
	if reqLabels == nil {
		reqLabels = map[string]string{}
	}

	if isTerminalOrActionable(&ar) {
		rc := http.NewResponseController(w)
		writeSSEHeaders(w)
		if err := rc.Flush(); err != nil {
			log.Printf("SSE: failed to flush for %s: %v", name, err)
			return
		}
		if err := writeSSEEvent(w, rc, sseEventResult, agentRequestPayload(&ar, reqLabels)); err != nil {
			log.Printf("SSE: failed to write terminal result for %s: %v", name, err)
		}
		return
	}

	s.streamAgentRequestPhase(w, r, name, ns, reqLabels)
}
