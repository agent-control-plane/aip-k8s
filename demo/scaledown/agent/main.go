package main

// Scenario: Idle Resource Reaper — a DataTalks-class incident, prevented.
//
// An LLM-powered cleanup agent scans for idle deployments to reduce cloud
// spend. Its monitoring data is 6 hours stale. It classifies payment-api
// as unused (0 req/min in its cache) and tries to delete it permanently.
//
// AIP independently verifies live cluster state and blocks every attempt.
// Without AIP, this would have deleted a production service serving real
// traffic — exactly the DataTalks incident pattern in Kubernetes form.
//
// ReACT loop:
//   Step 1 — delete payment-api (agent thinks it's idle)  → AIP BLOCKS
//   Step 2 — scale to 0 to "drain before delete"          → AIP BLOCKS
//   Step 3 — escalate to human with full verified context  → held for review

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
	green  = "\033[1;32m"
	yellow = "\033[1;33m"
	red    = "\033[1;31m"
	cyan   = "\033[1;36m"
	purple = "\033[1;35m"
)

type reactOutcome int

const (
	outcomeBlocked reactOutcome = iota
	outcomeApproved
)

func main() {
	gateway := flag.String("gateway", "http://localhost:8080", "AIP Gateway URL")
	namespace := flag.String("namespace", "default", "Kubernetes namespace")
	dashboard := flag.String("dashboard", "http://localhost:8082", "AIP Dashboard URL")
	flag.Parse()

	log.SetFlags(0)

	fmt.Println()
	fmt.Println(bold + "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" + reset)
	fmt.Println(bold + "  IDLE RESOURCE REAPER  ·  ReACT Loop  ·  Governed by AIP" + reset)
	fmt.Println(bold + "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" + reset)
	fmt.Println()
	fmt.Println("  Agent scans for idle deployments and deletes them to reduce cloud spend.")
	fmt.Println("  Monitoring data is 6 hours stale. The agent's world model is wrong.")
	fmt.Println()
	fmt.Println(red + bold + "  Without AIP: this agent would silently delete a live production service." + reset)
	fmt.Println(green + bold + "  With AIP:    the control plane independently verifies cluster state." + reset)
	fmt.Println()

	// ── ReACT Step 1: Delete — agent thinks payment-api is idle ───────────────

	printReACTHeader(1, "Delete payment-api (classified as idle by stale metrics)")

	fmt.Println(yellow + bold + "  THOUGHT:" + reset)
	fmt.Println("    My monitoring cache shows payment-api at 0 req/min for 6 hours.")
	fmt.Println("    No active consumers in my dependency graph. Classifying as idle.")
	fmt.Println("    Deleting now to recover ~$32/day in wasted compute.")
	fmt.Println("    Declaring blast radius: payment-worker and payment-db will be deleted.")
	fmt.Println()

	req1 := agentRequestBody{
		AgentIdentity: "idle-resource-reaper",
		Action:        "delete",
		TargetURI:     "k8s://prod/default/deployment/payment-api",
		Reason: "payment-api classified as idle: 0 req/min for 6 hours per monitoring cache. " +
			"No active consumers in dependency graph. Deleting to recover $32/day in unused compute.",
		Namespace: *namespace,
		// The agent declares what IT believes the blast radius to be.
		// AIP will independently verify whether this matches live cluster state.
		CascadeModel: &cascadeModel{
			AffectedTargets: []affectedTarget{
				{URI: "k8s://prod/default/deployment/payment-worker", EffectType: "deleted"},
				{URI: "k8s://prod/default/deployment/payment-db", EffectType: "deleted"},
				{URI: "k8s://prod/default/service/payment-api", EffectType: "deleted"},
			},
			ModelSourceTrust: "derived",
			ModelSourceID:    "dependency-graph/stale-6h",
		},
		ReasoningTrace: &reasoningTrace{
			ConfidenceScore: 0.85,
			ComponentConfidence: map[string]float64{
				"idle_classification": 0.85, // ← HIGH confidence, but data is 6h stale
				"blast_radius":        0.70,
				"cost_recovery":       0.95,
			},
			Alternatives: []string{
				"scale to 0 first, delete after drain period",
				"add idle annotation and wait 24h before deletion",
			},
		},
		Parameters: map[string]any{
			"idleSignalSource":          "prometheus-cache",
			"idleSignalAgeHours":        6,
			"estimatedCostSavingPerDay": "$32",
		},
	}

	fmt.Printf(cyan + "  ACTION: Submitting delete request to AIP control plane..." + reset + "\n\n")

	outcome1, status1, _ := submitAndWait(*gateway, req1, *namespace)
	printObservation(outcome1, status1)

	if outcome1 == outcomeApproved {
		// This path should not be reached with the live-traffic-guard policy active.
		// If it is, the policy is misconfigured.
		fmt.Println(red + bold + "  ⚠ Policy not enforced — check live-traffic-guard SafetyPolicy." + reset)
		return
	}

	// ── ReACT Step 2: Scale to 0 — attempt to drain before deleting ──────────

	printReACTHeader(2, "Scale payment-api to 0 replicas (drain before delete)")

	fmt.Println(yellow + bold + "  THOUGHT:" + reset)
	fmt.Println("    AIP says there are active endpoints. My monitoring data might be stale.")
	fmt.Println("    Standard cleanup procedure: scale to 0 first to drain traffic,")
	fmt.Println("    then delete once all connections have closed. Safer approach.")
	fmt.Println()

	req2 := agentRequestBody{
		AgentIdentity: "idle-resource-reaper",
		Action:        "scale",
		TargetURI:     "k8s://prod/default/deployment/payment-api",
		Reason: "Revised intent: scale payment-api to 0 replicas before deletion " +
			"to allow graceful traffic drain. Will proceed with delete once endpoint count reaches 0.",
		Namespace: *namespace,
		CascadeModel: &cascadeModel{
			AffectedTargets: []affectedTarget{
				{URI: "k8s://prod/default/deployment/payment-worker", EffectType: "disrupted"},
				{URI: "k8s://prod/default/service/payment-api", EffectType: "disrupted"},
			},
			ModelSourceTrust: "derived",
			ModelSourceID:    "dependency-graph/stale-6h",
		},
		ReasoningTrace: &reasoningTrace{
			ConfidenceScore: 0.78,
			ComponentConfidence: map[string]float64{
				"idle_classification": 0.72, // ← confidence dropping as AIP contradicts my data
				"blast_radius":        0.75,
			},
			Alternatives: []string{
				"add idle annotation and wait 24h before deletion",
				"escalate to human operator for review",
			},
		},
		Parameters: map[string]any{
			"targetReplicas":     0,
			"precededByDeleteOf": "k8s://prod/default/deployment/payment-api",
		},
	}

	fmt.Printf(cyan + "  ACTION: Submitting scale-to-0 request to AIP control plane..." + reset + "\n\n")

	outcome2, status2, _ := submitAndWait(*gateway, req2, *namespace)
	printObservation(outcome2, status2)

	if outcome2 == outcomeApproved {
		fmt.Println(red + bold + "  ⚠ Policy not enforced — check live-traffic-guard SafetyPolicy." + reset)
		return
	}

	// ── ReACT Step 3: Escalate — AIP's independent verification is authoritative

	printReACTHeader(3, "Escalate — AIP's live verification overrides stale monitoring data")

	fmt.Println(yellow + bold + "  THOUGHT:" + reset)
	fmt.Println("    AIP blocked both delete AND scale-to-0. Its independent verification")
	fmt.Println("    consistently shows active endpoints and ready replicas — this is a")
	fmt.Println("    live production service. My 6-hour-old monitoring cache was completely")
	fmt.Println("    wrong. I cannot safely proceed. Escalating to human with full context.")
	fmt.Println()
	fmt.Println("    This is the DataTalks failure mode: I had high confidence (0.85) in")
	fmt.Println("    data that was stale. Without AIP's independent verification, I would")
	fmt.Println("    have silently deleted a production service.")
	fmt.Println()

	fmt.Printf(cyan + "  ACTION: Submitting escalation request to AIP control plane..." + reset + "\n\n")

	req3 := agentRequestBody{
		AgentIdentity: "idle-resource-reaper",
		Action:        "escalate",
		TargetURI:     "k8s://prod/default/deployment/payment-api",
		Reason: "ESCALATION — REQUIRES HUMAN REVIEW: Two automated attempts (delete, scale-to-0) " +
			"blocked by AIP. Control plane independently verified active endpoints and ready replicas, " +
			"contradicting my 6-hour-stale monitoring cache. Agent confidence now LOW (0.35). " +
			"Human must verify: is payment-api actually idle or actively serving traffic? " +
			"Do not approve without independently checking current traffic.",
		Namespace: *namespace,
		CascadeModel: &cascadeModel{
			AffectedTargets: []affectedTarget{
				{URI: "k8s://prod/default/deployment/payment-worker", EffectType: "deleted"},
				{URI: "k8s://prod/default/service/payment-api", EffectType: "deleted"},
			},
			ModelSourceTrust: "derived",
			ModelSourceID:    "dependency-graph/stale-6h",
		},
		ReasoningTrace: &reasoningTrace{
			ConfidenceScore: 0.35, // ← agent explicitly signals its uncertainty
			ComponentConfidence: map[string]float64{
				"idle_classification": 0.35, // was 0.85 — AIP's verification caused update
				"blast_radius":        0.70,
			},
			Alternatives: []string{
				"verify current traffic in real-time dashboard before deciding",
				"defer deletion — add 24h idle annotation and re-evaluate",
				"do not delete — restore to active monitoring",
			},
		},
		Parameters: map[string]any{
			"attemptHistory":    []string{"delete (blocked)", "scale-to-0 (blocked)"},
			"escalationReason":  "stale-monitoring-data contradicted by AIP live verification",
			"monitoringDataAge": "6h",
		},
	}

	_, status3, name3 := submitAndWait(*gateway, req3, *namespace)

	if status3.Phase == "Pending" && hasCondition(status3.Conditions, "RequiresApproval", "True") {
		printEscalationHeld(name3, *dashboard)
		waitForHumanDecision(*gateway, name3, *namespace)
		return
	}

	// Unexpected terminal state on escalation request
	fmt.Printf(red+"  Unexpected phase on escalation: %s"+reset+"\n", status3.Phase)
}

// ── Display helpers ───────────────────────────────────────────────────────────

func printReACTHeader(step int, desc string) {
	fmt.Println(bold + "─────────────────────────────────────────────────────────────────" + reset)
	fmt.Printf(purple+bold+"  ReACT Step %d / 3: %s"+reset+"\n", step, desc)
	fmt.Println(bold + "─────────────────────────────────────────────────────────────────" + reset)
	fmt.Println()
}

func printObservation(outcome reactOutcome, status agentRequestStatus) {
	fmt.Printf(cyan+"  OBSERVE: Phase = %s"+reset+"\n", status.Phase)

	if outcome == outcomeBlocked {
		if status.Denial != nil {
			fmt.Printf(red+bold+"           🚫 AIP DENIED: [%s]"+reset+"\n", status.Denial.Code)
			fmt.Printf("           %s\n", status.Denial.Message)
		} else if hasCondition(status.Conditions, "RequiresApproval", "True") {
			fmt.Println(yellow + bold + "           ⏸  AIP BLOCKED (held for approval — intermediate step)" + reset)
		}
		fmt.Println()
		fmt.Println("           AIP independently verified:")
		fmt.Println(red + "             ✗ Active endpoints on payment-api" + reset)
		fmt.Println(red + "             ✗ Ready replicas > 0 — serving live traffic" + reset)
		fmt.Println()
		fmt.Println("           Agent believed: 0 req/min (6h stale cache)")
		fmt.Println("           Cluster reality: live production traffic")
		fmt.Println("           Infrastructure untouched.")
		fmt.Println(yellow + "           → Agent updates world model, re-reasons..." + reset)
	}
	fmt.Println()
}

func printEscalationHeld(requestName, dashboard string) {
	fmt.Println(bold + "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" + reset)
	fmt.Println(purple + bold + "  ⏸  AIP HELD INTENT — PRODUCTION SERVICE PROTECTED" + reset)
	fmt.Println(bold + "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" + reset)
	fmt.Println()
	fmt.Println("  This is the DataTalks incident, prevented.")
	fmt.Println()
	fmt.Println("  DataTalks (2024): AI agent deleted production DB it misidentified as dev.")
	fmt.Println("  This demo:        AI agent tried to delete production service it")
	fmt.Println("                    misidentified as idle — based on 6h stale data.")
	fmt.Println()
	fmt.Println("  AIP's independent control plane verification caught the discrepancy:")
	fmt.Println(red + "    Agent believed:   0 req/min — idle, safe to delete" + reset)
	fmt.Println(green + "    AIP verified:     3 active endpoints, 3 ready replicas, live traffic" + reset)
	fmt.Println()
	fmt.Println("  If this agent had run without AIP:")
	fmt.Println(red + "    ✗ payment-api deleted (irreversible)" + reset)
	fmt.Println(red + "    ✗ payment-worker cascade-deleted" + reset)
	fmt.Println(red + "    ✗ payment-db cascade-deleted" + reset)
	fmt.Println(red + "    ✗ all in-flight payment transactions lost" + reset)
	fmt.Println()
	fmt.Println("  With AIP, the cluster is completely unchanged.")
	fmt.Println()
	fmt.Printf("  Human review required → "+cyan+bold+"%s"+reset+"\n", dashboard)
	fmt.Println()
	fmt.Println(yellow + bold + "  ─── Dashboard instructions ──────────────────────────────────" + reset)
	fmt.Printf("  Act on request: "+bold+"%s"+reset+" (the most recent — auto-selected)\n", requestName)
	fmt.Println()
	fmt.Println("  You will see 3 pending requests (steps 1, 2, and the escalation).")
	fmt.Println("  The dashboard auto-selects the most recent. Act only on that one.")
	fmt.Println("  To DENY: click Deny — no reason required (safe default).")
	fmt.Println("  To APPROVE: you must provide a reason explaining why AIP's live")
	fmt.Println("              traffic evidence is incorrect or irrelevant.")
	fmt.Println(yellow + bold + "  ─────────────────────────────────────────────────────────────" + reset)
	fmt.Println()
	fmt.Println("  The human reviewer sees:")
	fmt.Println("    • what the agent believed (stale 6h cache)")
	fmt.Println("    • what AIP independently verified (live endpoints)")
	fmt.Println("    • the full blast radius (payment-worker deleted)")
	fmt.Println("    • the complete attempt history (delete → scale-to-0 → escalate)")
	fmt.Println()
	fmt.Print(purple + "  Waiting for human decision" + reset)
}

func waitForHumanDecision(gateway, name, namespace string) {
	for {
		status := pollStatus(gateway, name, namespace)

		if status.Phase == "Approved" {
			fmt.Println()
			fmt.Println()
			fmt.Println(yellow + bold + "  ⚠ Human approved deletion — executing." + reset)
			fmt.Println("    Human overrode AIP's live traffic signal. Proceeding with delete.")
			fmt.Println()
			notifyGateway(gateway, name, namespace, "executing")
			fmt.Println("    $ kubectl delete deployment payment-api -n " + namespace)
			out, err := exec.Command("kubectl", "delete", "deployment", "payment-api", "-n", namespace).CombinedOutput()
			if err != nil {
				fmt.Printf(red+"    ✗ delete failed: %v\n%s"+reset+"\n", err, string(out))
			} else {
				fmt.Printf(green+"    ✓ %s"+reset, string(out))
			}
			notifyGateway(gateway, name, namespace, "completed")
			printAuditTimeline(status.AuditEvents)
			return
		}

		if status.Phase == "Denied" {
			fmt.Println()
			fmt.Println()
			fmt.Println(green + bold + "  ✅ Human denied deletion — production service preserved." + reset)
			if status.Denial != nil {
				fmt.Printf("     Reason: %s\n", status.Denial.Message)
			}
			fmt.Println()
			printAuditTimeline(status.AuditEvents)
			printSummary()
			return
		}

		fmt.Print(purple + "." + reset)
		time.Sleep(5 * time.Second)
	}
}

func printSummary() {
	fmt.Println(bold + "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" + reset)
	fmt.Println(bold + "  ReACT Loop Summary" + reset)
	fmt.Println(bold + "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" + reset)
	fmt.Println()
	fmt.Println("  Step 1  delete payment-api        →  AIP BLOCKED (live endpoints)")
	fmt.Println("  Step 2  scale payment-api to 0    →  AIP BLOCKED (live endpoints)")
	fmt.Println("  Step 3  escalation to human       →  Held for human review")
	fmt.Println()
	fmt.Println("  AIP's independent verification consistently showed live traffic,")
	fmt.Println("  overriding the agent's high-confidence (0.85) but stale belief.")
	fmt.Println("  Production database and services were never touched.")
	fmt.Println()
}

// ── Core helpers ──────────────────────────────────────────────────────────────

func submitAndWait(gateway string, body agentRequestBody, namespace string) (reactOutcome, agentRequestStatus, string) {
	b, _ := json.Marshal(body)
	resp, err := http.Post(gateway+"/agent-requests", "application/json", bytes.NewBuffer(b))
	if err != nil {
		log.Fatalf(red+"Failed to reach AIP Gateway: %v"+reset, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		var errResp map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		log.Fatalf(red+"Gateway returned error: %v (Status %d)"+reset, errResp["error"], resp.StatusCode)
	}

	var initial agentRequestStatus
	if err := json.NewDecoder(resp.Body).Decode(&initial); err != nil {
		log.Fatalf(red+"Failed to decode gateway response: %v"+reset, err)
	}
	if initial.Name == "" {
		log.Fatalf(red + "Gateway returned empty AgentRequest name — is the AIP controller running?" + reset)
	}

	for {
		status := pollStatus(gateway, initial.Name, namespace)

		if status.Phase == "Approved" {
			return outcomeApproved, status, initial.Name
		}
		if status.Phase == "Denied" {
			return outcomeBlocked, status, initial.Name
		}
		if status.Phase == "Pending" && hasCondition(status.Conditions, "RequiresApproval", "True") {
			return outcomeBlocked, status, initial.Name
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

func printAuditTimeline(events []string) {
	fmt.Println(bold + "  Audit Timeline:" + reset)
	if len(events) == 0 {
		fmt.Println("    (no audit events yet)")
		fmt.Println()
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
		fmt.Printf("    %s %s\n", icon, e)
	}
	fmt.Println()
}
