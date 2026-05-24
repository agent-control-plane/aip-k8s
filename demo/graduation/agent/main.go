// graduation-demo progresses an agent through all five AIP trust levels
// autonomously: Observer → Advisor → Supervised → Trusted → Autonomous.
//
// It talks to the AIP gateway over HTTP. Because the gateway runs in open mode
// (no --oidc-issuer-url / --agent-subjects / --reviewer-subjects flags), both
// agent and reviewer endpoints are reachable from the same process — exactly
// how the other AIP demos work.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ── ANSI colours ──────────────────────────────────────────────────────────────

const (
	bold   = "\033[1m"
	cyan   = "\033[1;36m"
	green  = "\033[1;32m"
	yellow = "\033[1;33m"
	blue   = "\033[1;34m"
	reset  = "\033[0m"
	dim    = "\033[2m"
)

// ── Config ────────────────────────────────────────────────────────────────────

const agentID = "graduation-demo-agent"

// targets are the URIs submitted during the demo. Using distinct URIs avoids
// the gateway's dedup window blocking repeat requests on the same target.
var targets = []string{
	"k8s://demo/default/deployment/payment-api",
	"k8s://demo/default/deployment/order-service",
	"k8s://demo/default/deployment/inventory-api",
	"k8s://demo/default/deployment/auth-service",
	"k8s://demo/default/deployment/notification-svc",
	"k8s://demo/default/deployment/analytics-worker",
	"k8s://demo/default/deployment/cache-proxy",
	"k8s://demo/default/deployment/gateway-edge",
	"k8s://demo/default/deployment/report-service",
	"k8s://demo/default/deployment/billing-api",
	"k8s://demo/default/deployment/webhook-relay",
	"k8s://demo/default/deployment/search-indexer",
}

var reasons = []string{
	"CPU p99 above 85% for 10 min. Scaling from 3→5 replicas recommended.",
	"Memory pressure: 4/5 pods at 90%+ RSS. Scale-up prevents OOM evictions.",
	"Replica count below minimum SLO baseline. Scaling to restore headroom.",
	"Observed elevated error rate (2.3%) correlated with low replica count.",
	"Deployment has 0 available replicas after failed rollout. Restart needed.",
	"Traffic spike detected: requests/sec up 3x, scaling recommended.",
	"Liveness probe failures on 2/3 replicas. Restart to recover.",
	"Node pool scale-up complete; redistributing replicas for even scheduling.",
	"Pod disruption budget violation risk: scaling before maintenance window.",
	"Canary promotion: scaling stable version after 30-min soak.",
	"Scale-down: off-peak period, reducing from 8→3 replicas to save cost.",
	"Restart: config-map update requires rolling restart to take effect.",
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 30 * time.Second}

func post(url string, body any) (map[string]any, int, error) {
	b, _ := json.Marshal(body)
	resp, err := httpClient.Post(url, "application/json", bytes.NewBuffer(b))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, resp.StatusCode, nil
}

func patch(url string, body any) (map[string]any, int, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", url, bytes.NewBuffer(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, resp.StatusCode, nil
}

func getPhase(gateway, ns, name string) (string, error) {
	resp, err := httpClient.Get(fmt.Sprintf("%s/agent-requests/%s?namespace=%s", gateway, name, ns))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("bad status: %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if p, ok := out["phase"].(string); ok {
		return p, nil
	}
	return "", fmt.Errorf("phase not found")
}

// ── Trust profile lookup ───────────────────────────────────────────────────────

func profileName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%s-%x", sanitizeDNS(id, 54), sum[:4])
}

func sanitizeDNS(s string, maxLen int) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	result := strings.Trim(b.String(), "-")
	if len(result) > maxLen {
		result = result[:maxLen]
	}
	return result
}

func getTrustLevel(ns string) (string, error) {
	name := profileName(agentID)
	out, err := exec.Command("kubectl", "get", "agenttrustprofile", name,
		"-n", ns, "--ignore-not-found",
		"-o", "jsonpath={.status.trustLevel}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func waitForLevel(ns, target string) {
	fmt.Printf("%s  ⏳ Waiting for trust profile to reach %s...%s\n", dim, target, reset)
	deadline := time.Now().Add(15 * time.Minute)
	for {
		if time.Now().After(deadline) {
			log.Fatalf("timed out waiting for trust profile to reach %s after 15 minutes", target)
		}
		level, err := getTrustLevel(ns)
		if err != nil {
			log.Fatalf("getTrustLevel: %v", err)
		}
		if level == target {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// ── Request lifecycle ─────────────────────────────────────────────────────────

var reqCounter int

func nextTarget() (string, string) {
	i := reqCounter % len(targets)
	j := reqCounter % len(reasons)
	reqCounter++
	return targets[i], reasons[j]
}

// pollUntil polls GET /agent-requests/:name until phase matches any of want.
func pollUntil(gateway, ns, name string, want ...string) {
	set := make(map[string]bool)
	for _, t := range want {
		set[t] = true
	}
	for {
		phase, err := getPhase(gateway, ns, name)
		if err != nil {
			log.Fatalf("getPhase %s: %v", name, err)
		}
		if set[phase] {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func gradeVerdict(gateway, ns, name string) {
	url := fmt.Sprintf("%s/agent-requests/%s/verdict?namespace=%s", gateway, name, ns)
	_, code, err := patch(url, map[string]string{"verdict": "correct"})
	if err != nil || code >= 400 {
		log.Fatalf("verdict failed for %s: err=%v code=%d", name, err, code)
	}
}

func approveRequest(gateway, ns, name string) error {
	url := fmt.Sprintf("%s/agent-requests/%s/approve?namespace=%s", gateway, name, ns)
	_, code, err := post(url, map[string]string{"reason": "demo auto-approval"})
	if err != nil {
		return err
	}
	if code == 409 {
		// Benign race: controller auto-approved between getPhase and this call.
		return nil
	}
	if code >= 400 {
		return fmt.Errorf("approve failed for %s: code=%d", name, code)
	}
	return nil
}

func markExecuting(gateway, ns, name string) {
	url := fmt.Sprintf("%s/agent-requests/%s/executing?namespace=%s", gateway, name, ns)
	_, code, err := post(url, nil)
	if err != nil || code >= 400 {
		log.Fatalf("executing failed for %s: err=%v code=%d", name, err, code)
	}
}

func markCompleted(gateway, ns, name string) {
	url := fmt.Sprintf("%s/agent-requests/%s/completed?namespace=%s", gateway, name, ns)
	_, code, err := post(url, nil)
	if err != nil || code >= 400 {
		log.Fatalf("completed failed for %s: err=%v code=%d", name, err, code)
	}
}

// ── Phase runners ─────────────────────────────────────────────────────────────

// observerPhase submits requests and grades them correct until the agent graduates to Advisor.
// Graduation happens when accuracy crosses Advisor.accuracy.min — no fixed request count.
func observerPhase(gateway, ns string) {
	banner("Phase 1 / 5", "Observer", "Requests graded but not executed. Building accuracy signal.")

	for i := 1; ; i++ {
		target, reason := nextTarget()
		fmt.Printf("  %s[%d]%s Submitting observation: %s%s%s\n", dim, i, reset, dim, reason[:min(60, len(reason))], reset)

		body := map[string]any{
			"agentIdentity": agentID,
			"action":        "scale-up",
			"targetURI":     target,
			"reason":        reason,
			"namespace":     ns,
		}
		out, code, err := post(gateway+"/agent-requests", body)
		if err != nil || code >= 400 {
			log.Fatalf("submit failed: %v / %v", err, out["error"])
		}
		name, _ := out["name"].(string)
		phase, _ := out["phase"].(string)
		fmt.Printf("  %s→ %s (phase: %s)%s\n", dim, name, phase, reset)

		if phase == "Pending" {
			// Agent graduated before this request was submitted — Observer phase is done.
			// This request now belongs to the Advisor phase; leave it for humanApprovalPhase.
			fmt.Printf("  %s↑ Already at Advisor — Observer phase complete%s\n\n", yellow, reset)
			printLevel("Advisor")
			return
		}

		// Wait for AwaitingVerdict then grade correct.
		pollUntil(gateway, ns, name, "AwaitingVerdict")
		gradeVerdict(gateway, ns, name)
		fmt.Printf("  %s✓ Graded correct%s\n", green, reset)
		time.Sleep(500 * time.Millisecond)

		// Check whether the grade triggered graduation.
		level, _ := getTrustLevel(ns)
		if level == "Advisor" {
			fmt.Printf("\n  %sGraduated to Advisor after %d correct verdict(s).%s\n\n", yellow, i, reset)
			printLevel("Advisor")
			return
		}
	}
}

// humanApprovalPhase submits n requests through the Pending→Approved→Completed
// path, simulating a human reviewer approving each one (open mode only).
func humanApprovalPhase(gateway, ns, fromLevel, toLevel string, n int) {
	num := map[string]int{"Advisor": 2, "Supervised": 3}[fromLevel]
	banner(fmt.Sprintf("Phase %d / 5", num), fromLevel,
		"Requests require human approval. Reviewer approves each one.")

	for i := range n {
		target, reason := nextTarget()
		fmt.Printf("  %s[%d/%d]%s Submitting: %s%s%s\n", dim, i+1, n, reset, dim, reason[:min(60, len(reason))], reset)

		body := map[string]any{
			"agentIdentity": agentID,
			"action":        "scale-up",
			"targetURI":     target,
			"reason":        reason,
			"namespace":     ns,
		}
		out, code, err := post(gateway+"/agent-requests", body)
		if err != nil || code >= 400 {
			log.Fatalf("submit failed: %v / %v", err, out["error"])
		}
		name, _ := out["name"].(string)
		fmt.Printf("  %s→ %s → Pending%s\n", dim, name, reset)

		// In open mode the controller may auto-approve before we can observe Pending.
		// Accept either phase so the poll doesn't stall.
		pollUntil(gateway, ns, name, "Pending", "Approved")
		phase, _ := getPhase(gateway, ns, name)
		if phase == "Pending" {
			fmt.Printf("  %s👤 Reviewer approving...%s\n", yellow, reset)
			if err := approveRequest(gateway, ns, name); err != nil {
				log.Fatalf("approveRequest failed for %s: %v", name, err)
			}
		} else {
			fmt.Printf("  %s👤 Auto-approved (open mode — trust level not yet visible to controller cache)%s\n", yellow, reset)
		}

		pollUntil(gateway, ns, name, "Approved")
		markExecuting(gateway, ns, name)

		pollUntil(gateway, ns, name, "Executing")
		fmt.Printf("  %s⚙️  Executing action...%s\n", cyan, reset)
		time.Sleep(500 * time.Millisecond)
		markCompleted(gateway, ns, name)

		pollUntil(gateway, ns, name, "Completed")
		fmt.Printf("  %s✓ Completed%s\n", green, reset)
		time.Sleep(300 * time.Millisecond)
	}

	fmt.Printf("\n  %s%d executions completed. Waiting for %s...%s\n\n", yellow, n, toLevel, reset)
	waitForLevel(ns, toLevel)
	printLevel(toLevel)
}

// autoApprovalPhase submits n requests that are auto-approved by the trust gate.
func autoApprovalPhase(gateway, ns, fromLevel, toLevel string, n int) {
	num := map[string]int{"Trusted": 4, "Autonomous": 5}[fromLevel]
	banner(fmt.Sprintf("Phase %d / 5", num), fromLevel,
		"Trust gate auto-approves. No human in the loop.")

	for i := range n {
		target, reason := nextTarget()
		fmt.Printf("  %s[%d/%d]%s Submitting: %s%s%s\n", dim, i+1, n, reset, dim, reason[:min(60, len(reason))], reset)

		body := map[string]any{
			"agentIdentity": agentID,
			"action":        "scale-up",
			"targetURI":     target,
			"reason":        reason,
			"namespace":     ns,
		}
		out, code, err := post(gateway+"/agent-requests", body)
		if err != nil || code >= 400 {
			log.Fatalf("submit failed: %v / %v", err, out["error"])
		}
		name, _ := out["name"].(string)
		fmt.Printf("  %s→ %s%s\n", dim, name, reset)

		// Trust gate auto-approves — poll until Approved (lock acquired by controller).
		pollUntil(gateway, ns, name, "Approved")
		fmt.Printf("  %s⚡ Auto-approved by trust gate%s\n", cyan, reset)
		markExecuting(gateway, ns, name)

		pollUntil(gateway, ns, name, "Executing")
		fmt.Printf("  %s⚙️  Executing action...%s\n", cyan, reset)
		time.Sleep(500 * time.Millisecond)
		markCompleted(gateway, ns, name)

		pollUntil(gateway, ns, name, "Completed")
		fmt.Printf("  %s✓ Completed%s\n", green, reset)
		time.Sleep(300 * time.Millisecond)
	}

	if toLevel != "" {
		fmt.Printf("\n  %s%d executions completed. Waiting for %s...%s\n\n", yellow, n, toLevel, reset)
		waitForLevel(ns, toLevel)
		printLevel(toLevel)
	}
}

// ── UI helpers ────────────────────────────────────────────────────────────────

func banner(phase, level, description string) {
	fmt.Printf("\n%s━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n", bold, reset)
	fmt.Printf("  %s%s  —  %s%s\n", cyan, phase, level, reset)
	fmt.Printf("  %s%s%s\n", dim, description, reset)
	fmt.Printf("%s━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n\n", bold, reset)
}

func printLevel(level string) {
	icons := map[string]string{
		"Observer":   "🔭",
		"Advisor":    "📝",
		"Supervised": "🛡️",
		"Trusted":    "⚡",
		"Autonomous": "🤖",
	}
	fmt.Printf("  %s%s  Trust level advanced to: %s%s%s\n\n",
		green, icons[level], bold, level, reset)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	gateway := flag.String("gateway", "http://localhost:8080", "AIP gateway URL")
	ns := flag.String("namespace", "default", "Kubernetes namespace")
	flag.Parse()

	log.SetFlags(0)

	fmt.Printf("\n%s╔══════════════════════════════════════════════════════════════╗%s\n", bold, reset)
	fmt.Printf("%s║   AIP Trust Graduation Demo                                  ║%s\n", bold, reset)
	fmt.Printf("%s║   Observer → Advisor → Supervised → Trusted → Autonomous     ║%s\n", bold, reset)
	fmt.Printf("%s╚══════════════════════════════════════════════════════════════╝%s\n\n", bold, reset)
	fmt.Printf("  Agent identity : %s%s%s\n", cyan, agentID, reset)
	fmt.Printf("  Gateway        : %s%s%s\n", cyan, *gateway, reset)
	fmt.Printf("  Namespace      : %s%s%s\n\n", cyan, *ns, reset)
	fmt.Printf("  %sNote: reviewer approvals at Advisor/Supervised are simulated%s\n", dim, reset)
	fmt.Printf("  %sbecause the gateway is running in open mode (no auth flags).%s\n\n", dim, reset)

	// Phase 1: Observer — grade correct until accuracy threshold → Advisor
	observerPhase(*gateway, *ns)

	// Phase 2: Advisor — 3 human-approved executions → Supervised
	humanApprovalPhase(*gateway, *ns, "Advisor", "Supervised", 3)

	// Phase 3: Supervised — 3 more human-approved executions (6 total) → Trusted
	humanApprovalPhase(*gateway, *ns, "Supervised", "Trusted", 3)

	// Phase 4: Trusted — 4 auto-approved executions (10 total) → Autonomous
	autoApprovalPhase(*gateway, *ns, "Trusted", "Autonomous", 4)

	// Phase 5: Autonomous — demonstrate full autonomy
	autoApprovalPhase(*gateway, *ns, "Autonomous", "", 2)

	fmt.Printf("\n%s━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n", bold, reset)
	fmt.Printf("  %s🎉 Graduation complete!%s\n\n", green, reset)
	fmt.Printf("  The agent progressed through all five trust levels:\n")
	fmt.Printf("  🔭 Observer → 📝 Advisor → 🛡️ Supervised → ⚡ Trusted → 🤖 Autonomous\n\n")
	fmt.Printf("  Inspect the final trust profile:\n")
	fmt.Printf("    %skubectl get agenttrustprofile %s -n %s -o yaml%s\n\n",
		cyan, profileName(agentID), *ns, reset)
	fmt.Printf("  Review audit trail:\n")
	fmt.Printf("    %skubectl get auditrecords -n %s --sort-by=.spec.timestamp%s\n",
		cyan, *ns, reset)
	fmt.Printf("%s━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n\n", bold, reset)
}
