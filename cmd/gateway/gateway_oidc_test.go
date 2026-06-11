package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
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
		oidcServer = newOIDCTestServer()

		binPath := projDir + "/bin/gateway"
		cmdPath := projDir + "/cmd/gateway"

		cmd := exec.Command("go", "build", "-o", binPath, cmdPath)
		cmd.Dir = projDir
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "failed to build gateway: %s", string(out))

		gwArgs := []string{
			"--addr=:" + gwPort,
			"--oidc-issuer-url=" + oidcServer.IssuerURL,
			"--oidc-audience=aip-gateway",
			"--agent-subjects=agent-sub,reviewer-sub",
			"--reviewer-subjects=reviewer-sub",
			"--unregistered-agent-policy=allow",
		}
		gwProc = exec.Command(binPath, gwArgs...)
		gwProc.Dir = projDir
		gwProc.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		gwProc.Stdout = GinkgoWriter
		gwProc.Stderr = GinkgoWriter
		Expect(gwProc.Start()).To(Succeed(), "failed to start OIDC gateway")

		Eventually(func() int {
			resp, err := http.Get("http://localhost:" + gwPort + "/healthz") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 60*time.Second, time.Second).Should(Equal(http.StatusOK))

		gwCleanup("default")

		By("creating SafetyPolicy that requires human approval for gw-human-action")
		policy := &v1alpha1.SafetyPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-require-human", Namespace: "default"},
			Spec: v1alpha1.SafetyPolicySpec{
				GovernedResourceSelector: metav1.LabelSelector{},
				Rules: []v1alpha1.Rule{
					{
						Name:       "require-human",
						Type:       "StateEvaluation",
						Action:     "RequireApproval",
						Expression: "true",
					},
				},
				FailureMode: strPtr("FailClosed"),
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())

		By("waiting for SafetyPolicy to be visible before sending requests")
		Eventually(func(g Gomega) {
			var sp v1alpha1.SafetyPolicy
			g.Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "gw-require-human", Namespace: "default"}, &sp),
			).To(Succeed())
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
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("Expired token -> 401", func() {
		token := oidcServer.mintToken("agent-sub", "aip-gateway", -5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests", "{}", token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("Wrong audience -> 401", func() {
		token := oidcServer.mintToken("agent-sub", "wrong-aud", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests", "{}", token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
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
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))

		var body map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		createdReqName, _ = body["name"].(string)
		Expect(createdReqName).NotTo(BeEmpty())
	})

	It("Valid agent token — GET /agent-requests -> 200", func() {
		token := oidcServer.mintToken("agent-sub", "aip-gateway", 5*time.Minute)
		resp, err := gwGetWithToken(gwPort, "/agent-requests", token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("Valid agent token on approve (wrong role) -> 403", func() {
		token := oidcServer.mintToken("agent-sub", "aip-gateway", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort,
			"/agent-requests/"+createdReqName+"/approve", `{"reason":"e2e agent approve"}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))

		b, _ := io.ReadAll(resp.Body)
		Expect(string(b)).To(ContainSubstring("reviewer role required"))
	})

	It("Valid reviewer token on approve — self-approval -> 403", func() {
		token := oidcServer.mintToken("reviewer-sub", "aip-gateway", 5*time.Minute)
		createResp, err := gwPostWithToken(gwPort, "/agent-requests", `{
			"agentIdentity": "reviewer-sub",
			"action":        "gw-human-action",
			"targetURI":     "k8s://dev/default/deployment/gw-oidc-self",
			"reason":        "self approval tests"
		}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer createResp.Body.Close() //nolint:errcheck
		Expect(createResp.StatusCode).To(Equal(http.StatusCreated))

		var body map[string]any
		Expect(json.NewDecoder(createResp.Body).Decode(&body)).To(Succeed())
		selfReqName, _ := body["name"].(string)
		Expect(selfReqName).NotTo(BeEmpty())

		aprResp, err := gwPostWithToken(gwPort,
			"/agent-requests/"+selfReqName+"/approve", `{"reason":"self approve"}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer aprResp.Body.Close() //nolint:errcheck
		Expect(aprResp.StatusCode).To(Equal(http.StatusForbidden))

		b, _ := io.ReadAll(aprResp.Body)
		Expect(string(b)).To(ContainSubstring("self-approval not permitted"))
	})

	It("Valid reviewer token on approve — different creator -> 200/409", func() {
		token := oidcServer.mintToken("reviewer-sub", "aip-gateway", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort,
			"/agent-requests/"+createdReqName+"/approve", `{"reason":"e2e review approve"}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(BeElementOf(http.StatusOK, http.StatusConflict))
	})

	It("Healthz unauthenticated -> 200", func() {
		resp, err := http.Get("http://localhost:" + gwPort + "/healthz") //nolint:noctx
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})
})
