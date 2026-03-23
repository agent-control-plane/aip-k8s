package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"time"
)

// ── AIP gateway request/response types ────────────────────────────────────────

type affectedTarget struct {
	URI        string `json:"uri"`
	EffectType string `json:"effectType"`
}

type cascadeModel struct {
	AffectedTargets  []affectedTarget `json:"affectedTargets"`
	ModelSourceTrust string           `json:"modelSourceTrust,omitempty"`
	ModelSourceID    string           `json:"modelSourceID,omitempty"`
}

type reasoningTrace struct {
	ConfidenceScore     float64            `json:"confidenceScore"`
	ComponentConfidence map[string]float64 `json:"componentConfidence,omitempty"`
	Alternatives        []string           `json:"alternatives,omitempty"`
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

type condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type denial struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type agentRequestStatus struct {
	Name        string      `json:"name"`
	Phase       string      `json:"phase"`
	Denial      *denial     `json:"denial,omitempty"`
	Conditions  []condition `json:"conditions"`
	AuditEvents []string    `json:"auditEvents"`
}

// ── Terminal colours ──────────────────────────────────────────────────────────

const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	blue   = "\033[1;34m"
	green  = "\033[1;32m"
	yellow = "\033[1;33m"
	red    = "\033[1;31m"
	cyan   = "\033[1;36m"
	purple = "\033[1;35m"
)

func main() {
	gateway := flag.String("gateway", "http://localhost:8080", "AIP Gateway URL")
	namespace := flag.String("namespace", "default", "Kubernetes namespace")
	dashboard := flag.String("dashboard", "http://localhost:8082", "AIP Dashboard URL")
	flag.Parse()

	log.SetFlags(0)

	fmt.Println()
	fmt.Println(bold + "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" + reset)
	fmt.Println(bold + "  COST OPTIMIZER AGENT  ·  Governed by AIP Control Plane" + reset)
	fmt.Println(bold + "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" + reset)
	fmt.Println()
	fmt.Println(bold + "Intent:" + reset)
	fmt.Printf("  Agent:    cost-optimizer\n")
	fmt.Printf("  Action:   scale-down\n")
	fmt.Printf("  Target:   k8s://prod/default/deployment/payment-api\n")
	fmt.Printf("  Replicas: 3 → 1\n")
	fmt.Printf("  Reason:   Reduce cloud spend during predicted low-traffic window\n")
	fmt.Println()
	fmt.Println(bold + "Declared downstream dependencies (agent causal model):" + reset)
	fmt.Println("  • k8s://prod/default/service/cost-explorer   [disrupted]")
	fmt.Println("  • k8s://prod/default/deployment/payment-worker [disrupted]")
	fmt.Println()
	fmt.Println(bold + "─────────────────────────────────────────────────────────────────" + reset)

	// ── Build AgentRequest ────────────────────────────────────────────────────

	body := agentRequestBody{
		AgentIdentity: "cost-optimizer",
		Action:        "scale-down",
		TargetURI:     "k8s://prod/default/deployment/payment-api",
		Reason: "Scale payment-api from 3 to 1 replica to reduce cloud spend" +
			" during predicted low-traffic window. Estimated saving: $24/day.",
		Namespace: *namespace,
		// Agent declares what it knows about downstream impact from its causal model.
		// AIP will independently verify live cluster state (endpoints, ready replicas).
		CascadeModel: &cascadeModel{
			AffectedTargets: []affectedTarget{
				{URI: "k8s://prod/default/service/cost-explorer", EffectType: "disrupted"},
				{URI: "k8s://prod/default/deployment/payment-worker", EffectType: "disrupted"},
			},
			ModelSourceTrust: "inferred",
			ModelSourceID:    "static-dependency-graph/v1",
		},
		ReasoningTrace: &reasoningTrace{
			ConfidenceScore: 0.72,
			ComponentConfidence: map[string]float64{
				"traffic_prediction": 0.60,
				"dependency_impact":  0.85,
				"cost_model":         0.90,
			},
			Alternatives: []string{
				"scale to 2 replicas instead of 1",
				"schedule scale-down for 2am off-peak",
				"enable HPA with min-replicas=1",
			},
		},
		Parameters: map[string]any{
			"currentReplicas":           3,
			"targetReplicas":            1,
			"estimatedCostSavingPerDay": "$24",
		},
	}

	b, _ := json.Marshal(body)

	fmt.Println()
	fmt.Printf(yellow + "Submitting intent to AIP control plane..." + reset + "\n")
	fmt.Println()

	resp, err := http.Post(*gateway+"/agent-requests", "application/json", bytes.NewBuffer(b))
	if err != nil {
		log.Fatalf(red+"Failed to reach AIP Gateway: %v"+reset, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var initial agentRequestStatus
	if err := json.NewDecoder(resp.Body).Decode(&initial); err != nil {
		log.Fatalf(red+"Failed to decode gateway response: %v"+reset, err)
	}

	name := initial.Name
	fmt.Printf(cyan+"[AIP] AgentRequest created: %s"+reset+"\n", name)

	// ── Poll for resolution ───────────────────────────────────────────────────

	lastPhase := ""
	requiresApprovalPrinted := false

	for {
		status := pollStatus(*gateway, name, *namespace)

		if status.Phase != lastPhase && status.Phase != "" {
			fmt.Printf(cyan+"[AIP] Phase: %s"+reset+"\n", status.Phase)
			lastPhase = status.Phase
		}

		// RequiresApproval — blocked by control plane
		if status.Phase == "Pending" && hasCondition(status.Conditions, "RequiresApproval", "True") {
			if !requiresApprovalPrinted {
				fmt.Println()
				fmt.Println(red + bold + "⛔ AIP BLOCKED — Live Traffic Detected by Control Plane" + reset)
				fmt.Println()
				fmt.Println("  The control plane independently checked the cluster:")
				fmt.Println("  " + red + "✗ Active endpoints detected on payment-api" + reset)
				fmt.Println("  " + red + "✗ Ready replicas: 3 (service is serving traffic)" + reset)
				fmt.Println()
				fmt.Println("  Scaling down would disrupt:")
				fmt.Println("    • cost-explorer      (declared by agent)")
				fmt.Println("    • payment-worker     (declared by agent)")
				fmt.Println()
				fmt.Println(bold + "─────────────────────────────────────────────────────────────────" + reset)
				fmt.Println(yellow + bold + "  ⏸  INTENT HELD — infrastructure has NOT been touched" + reset)
				fmt.Println()
				fmt.Println("  Proposed action (NOT YET EXECUTED):")
				fmt.Println(red + "    $ kubectl scale deployment payment-api --replicas=1 -n " + *namespace + reset)
				fmt.Println()
				fmt.Println("  Verify the object is unchanged (run in another terminal):")
				fmt.Println(cyan + "    $ kubectl get deployment payment-api -n " +
					*namespace + " -o jsonpath='{.spec.replicas}'" + reset)
				fmt.Println(cyan + "    3   ← still 3, agent has not acted" + reset)
				fmt.Println(bold + "─────────────────────────────────────────────────────────────────" + reset)
				fmt.Println()
				fmt.Printf("  "+bold+"Human override required → %s"+reset+"\n", *dashboard)
				fmt.Printf("  Request ID: %s\n", name)
				fmt.Println()
				fmt.Print(purple + "  Waiting for human decision" + reset)
				requiresApprovalPrinted = true
			} else {
				fmt.Print(purple + "." + reset)
			}
			time.Sleep(5 * time.Second)
			continue
		}

		// Approved — execute scale-down and signal lifecycle to gateway
		if status.Phase == "Approved" {
			fmt.Println()
			fmt.Println()
			fmt.Println(green + bold + "✅ AIP APPROVED — executing now" + reset)
			fmt.Println()
			notifyGateway(*gateway, name, *namespace, "executing")
			executeScaleDown(*namespace)
			notifyGateway(*gateway, name, *namespace, "completed")
			printAuditTimeline(status.AuditEvents)
			return
		}

		// Denied
		if status.Phase == "Denied" {
			fmt.Println()
			fmt.Println(red + bold + "🚫 AIP DENIED" + reset)
			if status.Denial != nil {
				fmt.Printf("  Code:    %s\n", status.Denial.Code)
				fmt.Printf("  Message: %s\n", status.Denial.Message)
			}
			fmt.Println()
			fmt.Println("  Suggested alternatives:")
			fmt.Println("    • Scale to 2 replicas instead of 1")
			fmt.Println("    • Schedule scale-down for 2am off-peak window")
			fmt.Println("    • Enable HPA with min-replicas=1 for auto-scaling")
			printAuditTimeline(status.AuditEvents)
			return
		}

		if status.Phase == "Completed" || status.Phase == "Failed" {
			printAuditTimeline(status.AuditEvents)
			return
		}

		time.Sleep(2 * time.Second)
	}
}

func notifyGateway(gateway, name, namespace, event string) {
	url := fmt.Sprintf("%s/agent-requests/%s/%s?namespace=%s", gateway, name, event, namespace)
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func pollStatus(gateway, name, namespace string) agentRequestStatus {
	url := fmt.Sprintf("%s/agent-requests/%s?namespace=%s", gateway, name, namespace)
	resp, err := http.Get(url)
	if err != nil {
		return agentRequestStatus{}
	}
	defer func() { _ = resp.Body.Close() }()
	var s agentRequestStatus
	_ = json.NewDecoder(resp.Body).Decode(&s)
	return s
}

func hasCondition(conditions []condition, condType, status string) bool {
	for _, c := range conditions {
		if c.Type == condType && c.Status == status {
			return true
		}
	}
	return false
}

func executeScaleDown(namespace string) {
	fmt.Println(bold + "Executing scale-down..." + reset)
	fmt.Println()

	cmd := exec.Command("kubectl", "scale", "deployment", "payment-api",
		"--replicas=1", "-n", namespace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf(red+"  ✗ Scale failed: %v\n  %s"+reset+"\n", err, string(out))
		return
	}

	fmt.Printf(green+"  $ kubectl scale deployment payment-api --replicas=1 -n %s"+reset+"\n", namespace)
	fmt.Printf(green+"  %s"+reset+"\n", string(out))
	fmt.Println(green + bold + "✅ payment-api scaled: 3 → 1 replica" + reset)
	fmt.Println()
}

func printAuditTimeline(events []string) {
	fmt.Println(bold + "Audit Timeline:" + reset)
	if len(events) == 0 {
		fmt.Println("  (no audit events yet)")
		return
	}
	icons := map[string]string{
		"request.submitted": "📥",
		"policy.evaluated":  "⚖️ ",
		"request.approved":  "✅",
		"request.denied":    "🚫",
		"request.executing": "⚙️ ",
		"request.completed": "✅",
		"lock.acquired":     "🔒",
		"lock.released":     "🔓",
		"lock.expired":      "⏰",
		"request.failed":    "❌",
	}
	for _, e := range events {
		icon := "○ "
		if i, ok := icons[e]; ok {
			icon = i
		}
		fmt.Printf("  %s %s\n", icon, e)
	}
	fmt.Println()
}
