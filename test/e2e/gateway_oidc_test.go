//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/agent-control-plane/aip-k8s/test/utils"
)

func gwPostWithToken(port, path, body, token string) (*http.Response, error) {
	url := "http://localhost:" + port + path
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	return client.Do(req)
}

func gwGetWithToken(port, path, token string) (*http.Response, error) {
	url := "http://localhost:" + port + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	return client.Do(req)
}

var _ = Describe("Phase 7: Gateway OIDC Authentication", Ordered, func() {
	var oidcServer *oidcTestServer
	var gwProc *exec.Cmd
	const gwPort = "18083"

	BeforeAll(func() {
		// 1. Start oidcTestServer
		oidcServer = newOIDCTestServer()

		projDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred(), "failed to get project dir")
		binPath := projDir + "/bin/gateway"
		cmdPath := projDir + "/cmd/gateway"

		By("ensuring governance CRDs are installed")
		if os.Getenv("HELM_DEPLOYED") != "true" {
			cmd := exec.Command("make", "install")
			cmd.Dir = projDir
			out, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "failed to install CRDs: %s", string(out))
		} else {
			By("skipping make install; HELM_DEPLOYED=true")
		}

		By("ensuring controller is deployed (skips if already running)")
		if os.Getenv("HELM_DEPLOYED") != "true" {
			checkCmd := exec.Command("kubectl", "get", "deployment",
				"aip-k8s-controller", "-n", "aip-k8s-system")
			if _, checkErr := utils.Run(checkCmd); checkErr != nil {
				cmd := exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
				cmd.Dir = projDir
				out, err := cmd.CombinedOutput()
				Expect(err).NotTo(HaveOccurred(), "failed to deploy controller: %s", string(out))
			}
		} else {
			By("skipping make deploy; HELM_DEPLOYED=true")
		}

		By("waiting for controller to be ready")
		Eventually(func(g Gomega) {
			readyCmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-n", "aip-k8s-system",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
			status, err := utils.Run(readyCmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"), "controller-manager pod not yet ready")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		// 2. Build gateway binary
		cmd := exec.Command("go", "build", "-o", binPath, cmdPath)
		cmd.Dir = projDir
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "failed to build gateway: %s", string(out))

		// 3. Start gateway subprocess
		gwArgs := []string{
			"--addr=:" + gwPort,
			"--oidc-issuer-url=" + oidcServer.IssuerURL,
			"--oidc-audience=aip-gateway",
			"--agent-subjects=agent-sub,reviewer-sub", // include reviewer-sub for self-approval test setup
			"--reviewer-subjects=reviewer-sub",
		}
		gwProc = exec.Command(binPath, gwArgs...)
		gwProc.Dir = projDir
		gwProc.Stdout = GinkgoWriter
		gwProc.Stderr = GinkgoWriter
		Expect(gwProc.Start()).To(Succeed(), "failed to start OIDC gateway")

		// 4. Poll /healthz until ready
		Eventually(func() int {
			resp, err := http.Get("http://localhost:" + gwPort + "/healthz") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

		gwCleanup("default")

		// Create the same SafetyPolicy used by Phase 6 so the controller sets
		// RequiresApproval on "gw-human-action" requests, allowing the gateway's
		// create handler to return 201 via the early-return path instead of
		// blocking until the 90s timeout.
		By("creating SafetyPolicy that requires human approval for gw-human-action")
		policyJSON := `{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind": "SafetyPolicy",
			"metadata": {"name": "gw-require-human", "namespace": "default"},
			"spec": {
				"governedResourceSelector": {},
				"rules": [{"name": "require-human", "type": "StateEvaluation",
				           "action": "RequireApproval", "expression": "true"}],
				"failureMode": "FailClosed"
			}
		}`
		applyCmd := exec.Command("kubectl", "apply", "-f", "-")
		applyCmd.Stdin = strings.NewReader(policyJSON)
		applyOut, applyErr := applyCmd.CombinedOutput()
		Expect(applyErr).NotTo(HaveOccurred(), "failed to create SafetyPolicy: %s", string(applyOut))

		By("waiting for SafetyPolicy to be visible before sending requests")
		Eventually(func() error {
			return exec.Command("kubectl", "get", "safetypolicy",
				"gw-require-human", "-n", "default").Run()
		}, 15*time.Second, time.Second).Should(Succeed())
	})

	AfterAll(func() {
		if gwProc != nil && gwProc.Process != nil {
			_ = gwProc.Process.Kill()
		}
		if oidcServer != nil {
			oidcServer.Close()
		}
		gwCleanup("default")
	})

	var createdReqName string

	It("Missing Bearer token -> 401", func() {
		resp, err := gwPostWithToken(gwPort, "/agent-requests", "{}", "")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("Expired token -> 401", func() {
		token := oidcServer.mintToken("agent-sub", "aip-gateway", -5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests", "{}", token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("Wrong audience -> 401", func() {
		token := oidcServer.mintToken("agent-sub", "wrong-aud", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests", "{}", token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("Valid agent token — POST /agent-requests -> 201", func() {
		token := oidcServer.mintToken("agent-sub", "aip-gateway", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests", `{
			"agentIdentity": "agent-sub",
			"action":        "gw-human-action",
			"targetURI":     "k8s://dev/default/deployment/gw-oidc-app",
			"reason":        "e2e tests"
		}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))

		var body map[string]interface{}
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		createdReqName, _ = body["name"].(string)
		Expect(createdReqName).NotTo(BeEmpty())
	})

	It("Valid agent token — GET /agent-requests -> 200", func() {
		token := oidcServer.mintToken("agent-sub", "aip-gateway", 5*time.Minute)
		resp, err := gwGetWithToken(gwPort, "/agent-requests", token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("Valid agent token on approve (wrong role) -> 403", func() {
		token := oidcServer.mintToken("agent-sub", "aip-gateway", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests/"+createdReqName+"/approve", `{"reason":"e2e agent approve"}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))

		b, _ := io.ReadAll(resp.Body)
		Expect(string(b)).To(ContainSubstring("reviewer role required"))
	})

	It("Valid reviewer token on approve — self-approval -> 403", func() {
		// Create request as reviewer-sub
		token := oidcServer.mintToken("reviewer-sub", "aip-gateway", 5*time.Minute)
		createResp, err := gwPostWithToken(gwPort, "/agent-requests", `{
			"agentIdentity": "reviewer-sub",
			"action":        "gw-human-action",
			"targetURI":     "k8s://dev/default/deployment/gw-oidc-self",
			"reason":        "self approval tests"
		}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer createResp.Body.Close()
		Expect(createResp.StatusCode).To(Equal(http.StatusCreated))

		var body map[string]interface{}
		Expect(json.NewDecoder(createResp.Body).Decode(&body)).To(Succeed())
		selfReqName, _ := body["name"].(string)
		Expect(selfReqName).NotTo(BeEmpty())

		// Try to approve own request
		aprResp, err := gwPostWithToken(gwPort, "/agent-requests/"+selfReqName+"/approve", `{"reason":"self approve"}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer aprResp.Body.Close()
		Expect(aprResp.StatusCode).To(Equal(http.StatusForbidden))

		b, _ := io.ReadAll(aprResp.Body)
		Expect(string(b)).To(ContainSubstring("self-approval not permitted"))
	})

	It("Valid reviewer token on approve — different creator -> 200/409", func() {
		token := oidcServer.mintToken("reviewer-sub", "aip-gateway", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests/"+createdReqName+"/approve", `{"reason":"e2e review approve"}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(BeElementOf(http.StatusOK, http.StatusConflict))
	})

	It("Healthz unauthenticated -> 200", func() {
		resp, err := http.Get("http://localhost:" + gwPort + "/healthz") //nolint:noctx
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})
})
