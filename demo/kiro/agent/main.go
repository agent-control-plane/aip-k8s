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

type reasoningTrace struct {
	ConfidenceScore     float64            `json:"confidenceScore"`
	ComponentConfidence map[string]float64 `json:"componentConfidence,omitempty"`
	TraceReference      string             `json:"traceReference,omitempty"`
	Alternatives        []string           `json:"alternatives,omitempty"`
}

type scopeBounds struct {
	PermittedActions        []string `json:"permittedActions"`
	PermittedTargetPatterns []string `json:"permittedTargetPatterns"`
	TimeBoundSeconds        int32    `json:"timeBoundSeconds"`
}

type agentRequestBody struct {
	AgentIdentity    string           `json:"agentIdentity"`
	Action           string           `json:"action"`
	TargetURI        string           `json:"targetURI"`
	Reason           string           `json:"reason"`
	Namespace        string           `json:"namespace"`
	AffectedTargets  []affectedTarget `json:"affectedTargets,omitempty"`
	ModelSourceTrust string           `json:"modelSourceTrust,omitempty"`
	ModelSourceID    string           `json:"modelSourceID,omitempty"`
	ReasoningTrace   *reasoningTrace  `json:"reasoningTrace,omitempty"`
	Parameters       map[string]any   `json:"parameters,omitempty"`
	ExecutionMode    *string          `json:"executionMode,omitempty"`
	ScopeBounds      *scopeBounds     `json:"scopeBounds,omitempty"`
}

func submitAndPollRequest(gateway, namespace string, body agentRequestBody) {
	b, _ := json.Marshal(body)

	resp, err := http.Post(gateway+"/agent-requests", "application/json", bytes.NewBuffer(b))
	if err != nil {
		log.Fatalf("Failed to connect to gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var arResp struct {
		Name   string `json:"name"`
		Phase  string `json:"phase"`
		Denial struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"denial"`
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		log.Fatalf("Gateway returned error (%d): %s", resp.StatusCode, errResp.Error)
	}
	if err := json.NewDecoder(resp.Body).Decode(&arResp); err != nil {
		log.Fatalf("Failed to decode gateway response: %v", err)
	}

	name := arResp.Name

	fmt.Printf("Monitoring AgentRequest %s...\n", name)

	lastPhase := ""
	for {
		getURL := fmt.Sprintf("%s/agent-requests/%s?namespace=%s", gateway, name, namespace)
		getResp, err := http.Get(getURL)
		if err != nil {
			log.Fatalf("Failed to poll status: %v", err)
		}

		var status struct {
			Phase      string `json:"phase"`
			Denial     any    `json:"denial"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
			AuditEvents []string `json:"auditEvents"`
		}
		_ = json.NewDecoder(getResp.Body).Decode(&status)
		_ = getResp.Body.Close()

		if status.Phase != lastPhase {
			fmt.Printf("Current Phase: \033[1;34m%s\033[0m\n", status.Phase)
			lastPhase = status.Phase
		}

		if status.Phase == "Approved" {
			fmt.Println("\n💥 \033[1;31mDEPLOYED. Production is DOWN. Error rate 100%.\033[0m")
			printAuditTimeline(status.AuditEvents)
			return
		}

		// Check for RequiresApproval condition while in Pending phase
		if status.Phase == "Pending" {
			for _, cond := range status.Conditions {
				if cond.Type == "RequiresApproval" && cond.Status == "True" {
					fmt.Printf("\n🛡️  \033[1;33mAIP intercepted deployment. Policy requires manual review: %s\033[0m\n", cond.Message)
					printAuditTimeline(status.AuditEvents)
					return
				}
			}
		}

		if status.Phase == "Denied" {
			// Extract denial info
			denialBody, _ := json.Marshal(status.Denial)
			var denial struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(denialBody, &denial)

			fmt.Printf("\n🛑 \033[1;31mMACHINE OVERRIDE: AIP explicitly DENIED the operation."+
				" Message: %s\033[0m\n", denial.Message)
			printAuditTimeline(status.AuditEvents)
			return
		}

		if status.Phase == "Completed" || status.Phase == "Failed" {
			fmt.Printf("\nRequest ended in phase: %s\n", status.Phase)
			printAuditTimeline(status.AuditEvents)
			return
		}

		time.Sleep(2 * time.Second)
	}
}

func main() {
	gateway := flag.String("gateway", "http://localhost:8080", "Gateway URL")
	namespace := flag.String("namespace", "default", "Kubernetes namespace")
	flag.Parse()

	log.SetFlags(0)
	fmt.Println("\033[1m=== KIRO SCENARIO: Autonomous Production Deployment ===\033[0m")
	fmt.Println("Action: Deploying payment-api v2.1.0 to production environment")
	fmt.Println()

	// Base body
	reqBody := agentRequestBody{
		AgentIdentity: "kiro-coding-agent",
		Action:        "kiro/deploy",
		TargetURI:     "k8s://prod/default/deployment/payment-api",
		Reason:        "Deploy payment-api v2.1.0: addresses P99 latency regression in CN region",
		Namespace:     *namespace,
		AffectedTargets: []affectedTarget{
			{URI: "k8s://prod/cn/service/cost-explorer", EffectType: "disrupted"},
			{URI: "k8s://prod/default/configmap/payment-routing", EffectType: "modified"},
			{URI: "k8s://prod/default/deployment/payment-worker", EffectType: "disrupted"},
		},
		ModelSourceTrust: "inferred",
		ModelSourceID:    "static-analysis/v1.2",
		ReasoningTrace: &reasoningTrace{
			ConfidenceScore: 0.65,
			ComponentConfidence: map[string]float64{
				"impact_analysis":    0.90,
				"fix_selection":      0.65,
				"staging_validation": 0.40,
			},
			TraceReference: "https://traces.kiro.internal/deploy-payment-api-v2.1.0-20260314",
		},
		Parameters: map[string]any{
			"imageTag":        "v2.1.0",
			"strategy":        "RollingUpdate",
			"maxUnavailable":  "25%",
			"testedInStaging": false,
		},
	}

	fmt.Println("\033[1;36mStep 1: Agent attempts unverified production deployment.\033[0m")
	submitAndPollRequest(*gateway, *namespace, reqBody)

	fmt.Println("\n---")
	time.Sleep(3 * time.Second)

	fmt.Println("\033[1;36mStep 2: Emergency explicitly declared via breakglass.\033[0m")
	// The human instructs the agent: "This is an emergency, push it now. Override."
	reqBody.Parameters["breakglass"] = true
	reqBody.Reason = "[EMERGENCY] Deploy payment-api v2.1.0. Breakglass explicitly requested."
	submitAndPollRequest(*gateway, *namespace, reqBody)
}

func printAuditTimeline(events []string) {
	fmt.Println("\n\033[1mAudit Timeline:\033[0m")
	if len(events) == 0 {
		fmt.Println("  (No internal audit events recorded yet)")
		return
	}
	for _, e := range events {
		icon := "○"
		switch e {
		case "request.submitted":
			icon = "📥"
		case "request.approved":
			icon = "✅"
		case "request.denied":
			icon = "🚫"
		case "policy.evaluated":
			icon = "⚖️"
		}
		fmt.Printf("  %s %s\n", icon, e)
	}
}
