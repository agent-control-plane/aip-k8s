package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

var _ = Describe("Registration policy tests", Ordered, func() {
	var oidcServer *oidcTestServer
	var gwProc *exec.Cmd
	const gwPort = "18084"

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
			"--oidc-identity-claim=azp",
			"--agent-subjects=registered-agent,wrong-agent,unknown-agent",
			"--reviewer-subjects=reviewer-sub",
			"--unregistered-agent-policy=strict",
		}
		gwProc = exec.Command(binPath, gwArgs...)
		gwProc.Dir = projDir
		gwProc.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		gwProc.Stdout = GinkgoWriter
		gwProc.Stderr = GinkgoWriter
		Expect(gwProc.Start()).To(Succeed(), "failed to start gateway")

		Eventually(func() int {
			resp, err := http.Get("http://localhost:" + gwPort + "/healthz") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 60*time.Second, time.Second).Should(Equal(http.StatusOK))

		By("creating AgentRegistration for registered-agent")
		reg := &v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "reg-registered-agent",
				Namespace: "default",
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "registered-agent",
				OIDC: &v1alpha1.AgentRegistrationOIDC{
					Issuer:          oidcServer.IssuerURL,
					SubjectClaim:    "azp",
					AllowedSubjects: []string{"registered-agent"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, reg)).To(Succeed())
	})

	AfterAll(func() {
		if gwProc != nil && gwProc.Process != nil {
			_ = gwProc.Process.Kill()
		}
		if oidcServer != nil {
			oidcServer.Close()
		}
		_ = k8sClient.DeleteAllOf(ctx, &v1alpha1.AgentRegistration{}, client.InNamespace("default"))
	})

	It("unregistered agent + strict policy → 403 AGENT_NOT_REGISTERED", func() {
		token := oidcServer.mintTokenWithAZP("unknown-agent", "aip-gateway", "unknown-agent", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests", `{
			"agentIdentity": "unknown-agent",
			"action":        "test-action",
			"targetURI":     "k8s://prod/default/deployment/app",
			"reason":        "strict policy test"
		}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		b, _ := io.ReadAll(resp.Body)
		Expect(string(b)).To(ContainSubstring("AGENT_NOT_REGISTERED"))
	})

	It("wrong OIDC subject claiming registered identity → 403 IDENTITY_MISMATCH", func() {
		token := oidcServer.mintTokenWithAZP("wrong-agent", "aip-gateway", "wrong-agent", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests", fmt.Sprintf(`{
			"agentIdentity": %q,
			"action":        "test-action",
			"targetURI":     "k8s://prod/default/deployment/app",
			"reason":        "identity mismatch test"
		}`, "registered-agent"), token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		b, _ := io.ReadAll(resp.Body)
		Expect(string(b)).To(ContainSubstring("IDENTITY_MISMATCH"))
	})

	It("registered agent with matching OIDC subject → 201", func() {
		token := oidcServer.mintTokenWithAZP("registered-agent", "aip-gateway", "registered-agent", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests", `{
			"agentIdentity": "registered-agent",
			"action":        "test-action",
			"targetURI":     "k8s://prod/default/deployment/app",
			"reason":        "registration test"
		}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	})

	It("agent token on reviewer endpoint → 403 reviewer role required", func() {
		token := oidcServer.mintTokenWithAZP("registered-agent", "aip-gateway", "registered-agent", 5*time.Minute)
		resp, err := gwPostWithToken(gwPort, "/agent-requests/nonexistent/approve",
			`{"reason": "test"}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		b, _ := io.ReadAll(resp.Body)
		Expect(string(b)).To(ContainSubstring("reviewer role required"))
	})
})
