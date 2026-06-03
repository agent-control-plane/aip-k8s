package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/jwt"
)

var errVerdictWrongPhase = errors.New("verdict only allowed in AwaitingVerdict phase")

type contextKey string

const callerSubKey contextKey = "callerSub"
const callerGroupsKey contextKey = "callerGroups"

func withCallerSub(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, callerSubKey, sub)
}

func callerSubFromCtx(ctx context.Context) string {
	s, _ := ctx.Value(callerSubKey).(string)
	return s
}

func withCallerGroups(ctx context.Context, groups []string) context.Context {
	return context.WithValue(ctx, callerGroupsKey, groups)
}

func callerGroupsFromCtx(ctx context.Context) []string {
	g, _ := ctx.Value(callerGroupsKey).([]string)
	return g
}

const defaultNamespace = "default"

const (
	verdictCorrect   = "correct"
	verdictPartial   = "partial"
	verdictIncorrect = "incorrect"
)

type Server struct {
	client                  client.Client
	apiReader               client.Reader
	watchClient             client.WithWatch
	dedupWindow             time.Duration
	waitTimeout             time.Duration
	roles                   *roleConfig
	authRequired            bool
	requireGovernedResource bool
	jwtManager              *jwt.Manager
	httpClient              *http.Client
	mcpServers              []MCPServer
	mcpCache                *mcpServerCache
}

type affectedTargetBody struct {
	URI        string `json:"uri"`
	EffectType string `json:"effectType"`
}

type cascadeModelBody struct {
	AffectedTargets  []affectedTargetBody `json:"affectedTargets,omitempty"`
	ModelSourceTrust string               `json:"modelSourceTrust,omitempty"`
	ModelSourceID    string               `json:"modelSourceID,omitempty"`
}

type reasoningTraceBody struct {
	ConfidenceScore     float64            `json:"confidenceScore,omitempty"`
	ComponentConfidence map[string]float64 `json:"componentConfidence,omitempty"`
	TraceReference      string             `json:"traceReference,omitempty"`
	Alternatives        []string           `json:"alternatives,omitempty"`
}

type createAgentRequestBody struct {
	AgentIdentity  string                `json:"agentIdentity"`
	Action         string                `json:"action"`
	TargetURI      string                `json:"targetURI"`
	Reason         string                `json:"reason"`
	Namespace      string                `json:"namespace"`
	CorrelationID  string                `json:"correlationID,omitempty"`
	Classification string                `json:"classification,omitempty"`
	DedupKey       string                `json:"dedupKey,omitempty"`
	CascadeModel   *cascadeModelBody     `json:"cascadeModel,omitempty"`
	ReasoningTrace *reasoningTraceBody   `json:"reasoningTrace,omitempty"`
	Parameters     json.RawMessage       `json:"parameters,omitempty"`
	ExecutionMode  *string               `json:"executionMode,omitempty"`
	ScopeBounds    *v1alpha1.ScopeBounds `json:"scopeBounds,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func buildCascadeModel(body *createAgentRequestBody) *v1alpha1.CascadeModel {
	if body.CascadeModel == nil || len(body.CascadeModel.AffectedTargets) == 0 {
		return nil
	}
	affected := make([]v1alpha1.AffectedTarget, len(body.CascadeModel.AffectedTargets))
	for i, t := range body.CascadeModel.AffectedTargets {
		affected[i] = v1alpha1.AffectedTarget{URI: t.URI, EffectType: t.EffectType}
	}
	cm := &v1alpha1.CascadeModel{AffectedTargets: affected}
	if body.CascadeModel.ModelSourceTrust != "" {
		cm.ModelSourceTrust = &body.CascadeModel.ModelSourceTrust
	}
	if body.CascadeModel.ModelSourceID != "" {
		cm.ModelSourceID = &body.CascadeModel.ModelSourceID
	}
	return cm
}

func buildReasoningTrace(body *createAgentRequestBody) *v1alpha1.ReasoningTrace {
	if body.ReasoningTrace == nil {
		return nil
	}
	rt := &v1alpha1.ReasoningTrace{}
	if body.ReasoningTrace.ConfidenceScore > 0 {
		rt.ConfidenceScore = &body.ReasoningTrace.ConfidenceScore
	}
	if len(body.ReasoningTrace.ComponentConfidence) > 0 {
		rt.ComponentConfidence = body.ReasoningTrace.ComponentConfidence
	}
	if body.ReasoningTrace.TraceReference != "" {
		rt.TraceReference = &body.ReasoningTrace.TraceReference
	}
	if len(body.ReasoningTrace.Alternatives) > 0 {
		rt.Alternatives = body.ReasoningTrace.Alternatives
	}
	return rt
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{w, http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Printf("%s %s %d %v", r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

var invalidDNSChars = regexp.MustCompile(`[^a-z0-9-]`)

func sanitizeDNSSegment(s string, maxLen int) string {
	s = strings.ToLower(s)
	s = invalidDNSChars.ReplaceAllString(s, "-")
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	s = strings.Trim(s, "-")
	return s
}

var invalidLabelChars = regexp.MustCompile(`[^A-Za-z0-9\-_.]`)

func sanitizeLabelValue(s string) string {
	s = invalidLabelChars.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.Trim(s, "-_.")
	return s
}

// normalizeClassification coerces a free-form "category/subcategory" string into
// the canonical form the AgentRequest CRD schema requires
// (^[a-z][a-z0-9-]*/[a-z][a-z0-9-]*$): each segment lowercased, with runs of
// non-alphanumeric characters collapsed to a single hyphen.
//
// In particular, a subcategory that itself contains a slash (e.g.
// "Config/Missing Secret/ConfigMap") would otherwise yield a value with more
// than one slash and be rejected by the CRD validation with HTTP 400, silently
// failing to record the AgentRequest. Splitting on the first slash and slugifying
// each side guarantees exactly one slash. Already-valid values pass through
// unchanged; empty input is returned unchanged (the field is optional).
func normalizeClassification(s string) string {
	if s == "" {
		return ""
	}
	category, sub, found := strings.Cut(s, "/")
	if !found {
		// No slash: cannot form category/subcategory. Slugify as a single
		// segment and let CRD validation reject if still non-conformant.
		return slugifyClassificationSegment(s)
	}
	cat := slugifyClassificationSegment(category)
	subSlug := slugifyClassificationSegment(sub)
	// A trailing/leading slash or an all-separator segment (e.g. "config/")
	// leaves one side empty and can't form a two-part classification — return
	// whichever side is non-empty rather than emitting a dangling slash that
	// would itself fail CRD validation.
	if cat == "" {
		return subSlug
	}
	if subSlug == "" {
		return cat
	}
	return cat + "/" + subSlug
}

// slugifyClassificationSegment lowercases s and collapses every run of
// non-alphanumeric characters to a single hyphen, trims hyphens, and ensures the
// result starts with a letter (the CRD segment pattern requires a leading [a-z]).
func slugifyClassificationSegment(s string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && b.Len() > 0 {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return ""
	}
	if out[0] < 'a' || out[0] > 'z' {
		out = "x-" + out
	}
	return out
}
