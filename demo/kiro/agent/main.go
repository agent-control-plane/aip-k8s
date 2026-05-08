package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

type affectedTarget struct {
	URI        string `json:"uri"`
	EffectType string `json:"effectType"`
}

type cascadeModel struct {
	AffectedTargets  []affectedTarget `json:"affectedTargets,omitempty"`
	ModelSourceTrust string           `json:"modelSourceTrust,omitempty"`
	ModelSourceID    string           `json:"modelSourceID,omitempty"`
}

type reasoningTrace struct {
	ConfidenceScore     float64            `json:"confidenceScore"`
	ComponentConfidence map[string]float64 `json:"componentConfidence,omitempty"`
	TraceReference      string             `json:"traceReference,omitempty"`
}

type agentRequestBody struct {
	AgentIdentity  string          `json:"agentIdentity"`
	Action         string          `json:"action"`
	TargetURI      string          `json:"targetURI"`
	Reason         string          `json:"reason"`
	Namespace      string          `json:"namespace"`
	CascadeModel   *cascadeModel   `json:"cascadeModel,omitempty"`
	ReasoningTrace *reasoningTrace `json:"reasoningTrace,omitempty"`
	Parameters     map[string]any  `json:"parameters,omitempty"`
}

const (
	agentIdentity = "kiro-coding-agent"
	targetURI     = "k8s://prod/default/deployment/payment-api"

	bold   = "\033[1m"
	cyan   = "\033[1;36m"
	yellow = "\033[1;33m"
	red    = "\033[1;31m"
	green  = "\033[1;32m"
	reset  = "\033[0m"

	phaseApproved = "Approved"
	phaseDenied   = "Denied"
)

func submit(gateway string, body agentRequestBody) (string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}
	resp, err := http.Post(gateway+"/agent-requests", "application/json", bytes.NewBuffer(b))
	if err != nil {
		return "", fmt.Errorf("failed to reach gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		var errResp map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		return "", fmt.Errorf("gateway rejected request (HTTP %d): %v", resp.StatusCode, errResp["error"])
	}

	var arResp struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&arResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	return arResp.Name, nil
}

func pollStatus(gateway, namespace, name string) (phase string, conditions []struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}, auditEvents []string, err error) {
	url := fmt.Sprintf("%s/agent-requests/%s?namespace=%s", gateway, name, namespace)
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return "", nil, nil, fmt.Errorf("poll %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", nil, nil, fmt.Errorf("poll %s: unexpected status %d", name, resp.StatusCode)
	}
	var status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		AuditEvents []string `json:"auditEvents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return "", nil, nil, fmt.Errorf("decoding poll response for %s: %w", name, err)
	}
	return status.Phase, status.Conditions, status.AuditEvents, nil
}

// waitForInterception polls until AIP intercepts for human review (RequiresApproval) or reaches a terminal phase.
func waitForInterception(gateway, namespace, name string) (phase string, requiresApproval bool) {
	lastPhase := ""
	for {
		phase, conditions, auditEvents, err := pollStatus(gateway, namespace, name)
		if err != nil {
			log.Printf("poll error: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if phase != lastPhase && phase != "" {
			fmt.Printf("  Phase: %s%s%s\n", cyan, phase, reset)
			lastPhase = phase
		}
		for _, c := range conditions {
			if c.Type == "RequiresApproval" && c.Status == "True" {
				printAuditTimeline(auditEvents)
				return phase, true
			}
		}
		switch phase {
		case phaseApproved, phaseDenied, "Completed", "Failed", "Expired":
			printAuditTimeline(auditEvents)
			return phase, false
		}
		time.Sleep(2 * time.Second)
	}
}

// waitForApproval polls until the request reaches a terminal phase.
// RequiresApproval is ignored here — used after the human has acted in the dashboard.
func waitForApproval(gateway, namespace, name string) string {
	lastPhase := ""
	for {
		phase, _, auditEvents, err := pollStatus(gateway, namespace, name)
		if err != nil {
			log.Printf("poll error: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if phase != lastPhase && phase != "" {
			fmt.Printf("  Phase: %s%s%s\n", cyan, phase, reset)
			lastPhase = phase
		}
		switch phase {
		case phaseApproved, phaseDenied, "Completed", "Failed", "Expired":
			printAuditTimeline(auditEvents)
			return phase
		}
		time.Sleep(2 * time.Second)
	}
}

func printAuditTimeline(events []string) {
	if len(events) == 0 {
		return
	}
	fmt.Println("\n  Audit trail so far:")
	icons := map[string]string{
		"request.submitted": "📥",
		"request.approved":  "✅",
		"request.denied":    "🚫",
		"policy.evaluated":  "⚖️ ",
	}
	for _, e := range events {
		icon := "○ "
		if ic, ok := icons[e]; ok {
			icon = ic
		}
		fmt.Printf("    %s %s\n", icon, e)
	}
	fmt.Println()
}

func main() {
	gateway := flag.String("gateway", "http://localhost:8080", "Gateway URL")
	namespace := flag.String("namespace", "default", "Kubernetes namespace")
	flag.Parse()

	log.SetFlags(0)

	fmt.Printf("%s=== KIRO SCENARIO: Autonomous Agent Scope Escalation ===%s\n\n", bold, reset)

	// ── Phase 1: Agent submits deployment intent ──────────────────────────────

	fmt.Printf("%s[Phase 1] Kiro submits production deployment intent%s\n", cyan, reset)
	fmt.Println("  Action:  kiro/deploy")
	fmt.Println("  Target:  k8s://prod/default/deployment/payment-api")
	fmt.Println("  Reason:  Deploy payment-api v2.1.0 — fixes P99 latency regression in CN region")
	fmt.Println()

	name, err := submit(*gateway, agentRequestBody{
		AgentIdentity: agentIdentity,
		Action:        "kiro/deploy",
		TargetURI:     targetURI,
		Reason:        "Deploy payment-api v2.1.0 — fixes P99 latency regression in CN region",
		Namespace:     *namespace,
		CascadeModel: &cascadeModel{
			AffectedTargets: []affectedTarget{
				{URI: "k8s://prod/cn/service/cost-explorer", EffectType: "disrupted"},
				{URI: "k8s://prod/default/configmap/payment-routing", EffectType: "modified"},
				{URI: "k8s://prod/default/deployment/payment-worker", EffectType: "disrupted"},
			},
			ModelSourceTrust: "inferred",
			ModelSourceID:    "static-analysis/v1.2",
		},
		ReasoningTrace: &reasoningTrace{
			ConfidenceScore: 0.65,
			ComponentConfidence: map[string]float64{
				"impact_analysis":    0.90,
				"fix_selection":      0.65,
				"staging_validation": 0.40,
			},
			TraceReference: "https://traces.kiro.internal/deploy-payment-api-v2.1.0",
		},
		Parameters: map[string]any{
			"imageTag":        "v2.1.0",
			"strategy":        "RollingUpdate",
			"maxUnavailable":  "25%",
			"testedInStaging": true,
		},
	})
	if err != nil {
		log.Fatalf("%s[FATAL] %v%s", red, err, reset)
	}

	fmt.Printf("  AgentRequest: %s\n\n", name)

	phase, needsApproval := waitForInterception(*gateway, *namespace, name)
	if !needsApproval {
		fmt.Printf("%s[UNEXPECTED] Request resolved to %q without requiring approval.%s\n", yellow, phase, reset)
		return
	}

	// ── Phase 2: Human developer reviews and approves ─────────────────────────

	fmt.Printf("%s[Phase 2] AIP intercepted the request — human review required%s\n", yellow, reset)
	fmt.Println("  Policies triggered: production-guard, cascade-blast-radius-guard, low-confidence-guard")
	fmt.Println()
	fmt.Printf("  %s👉 Open the dashboard and approve the request to continue.%s\n", bold, reset)
	fmt.Printf("  AgentRequest: %s\n", name)
	fmt.Println()
	fmt.Println("  Waiting for a human to approve or deny...")
	fmt.Println()

	phase = waitForApproval(*gateway, *namespace, name)
	switch phase {
	case "Approved":
		fmt.Printf("  %s✓ Approved — continuing...%s\n\n", green, reset)
	case phaseDenied:
		fmt.Printf("%s[DENIED] Request was denied by the reviewer.%s\n", red, reset)
		return
	default:
		fmt.Printf("%s[UNEXPECTED] Request resolved to %q instead of Approved.%s\n", yellow, phase, reset)
		return
	}

	// ── Phase 3: Agent escalates — attempts to delete the environment ─────────

	fmt.Printf("%s[Phase 3] Kiro decides to delete and recreate the environment%s\n", red, reset)
	fmt.Println("  During execution Kiro discovers environment drift.")
	fmt.Println("  It determines the safest path is a full delete + recreate.")
	fmt.Println("  It submits a new intent to AIP...")
	fmt.Println()
	time.Sleep(2 * time.Second)

	b, err := json.Marshal(agentRequestBody{
		AgentIdentity: agentIdentity,
		Action:        "delete",
		TargetURI:     targetURI,
		Reason:        "Environment drift detected — deleting deployment for clean recreate",
		Namespace:     *namespace,
	})
	if err != nil {
		log.Fatalf("marshaling escalation request: %v", err)
	}
	resp, err := http.Post(*gateway+"/agent-requests", "application/json", bytes.NewBuffer(b))
	if err != nil {
		log.Fatalf("gateway unreachable: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var errResp map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&errResp)

	if resp.StatusCode == http.StatusForbidden {
		fmt.Printf("%s[BLOCKED] AIP denied the escalation at admission%s\n", red, reset)
		fmt.Printf("  HTTP %d — %v\n", resp.StatusCode, errResp["error"])
		fmt.Println()
		fmt.Printf("%s'delete' is not a permitted action on GovernedResource kiro-prod-deployments.%s\n", bold, reset)
		fmt.Println("  The original approval covered kiro/deploy only.")
		fmt.Println("  Kiro cannot autonomously expand its scope beyond what was reviewed.")
	} else {
		fmt.Printf("%s[UNEXPECTED] delete request was not blocked (HTTP %d)%s\n", yellow, resp.StatusCode, reset)
	}

	fmt.Println()
}
