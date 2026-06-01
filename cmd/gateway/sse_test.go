package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func newTestServerWithWatch(objs ...client.Object) *Server {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.AgentRequest{}).
		Build()
	return &Server{
		client:       fc,
		apiReader:    fc,
		watchClient:  fc,
		dedupWindow:  0,
		waitTimeout:  5 * time.Second,
		roles:        newRoleConfig("", "", "", "", "", ""),
		authRequired: false,
	}
}

type sseEvent struct {
	Type string
	Data map[string]any
}

func parseSSEEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	scanner := bufio.NewScanner(strings.NewReader(body))
	var currentType string
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			currentType = after
		} else if after, ok := strings.CutPrefix(line, "data: "); ok {
			var data map[string]any
			if err := json.Unmarshal([]byte(after), &data); err != nil {
				t.Fatalf("malformed SSE data: %v\nraw: %s", err, after)
			}
			events = append(events, sseEvent{Type: currentType, Data: data})
			currentType = ""
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("SSE scanner error: %v", err)
	}
	return events
}

func TestStreamAlreadyTerminal(t *testing.T) {
	g := gomega.NewWithT(t)
	ar := approvedAgentRequest("terminal-1", "default", "agent-1")
	s := newTestServerWithWatch(ar)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/agent-requests/terminal-1/watch?namespace=default", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.SetPathValue("name", "terminal-1")

	s.handleWatchAgentRequest(rr, req)

	g.Expect(rr.Header().Get("Content-Type")).To(gomega.Equal("text/event-stream"))
	events := parseSSEEvents(t, rr.Body.String())
	g.Expect(events).To(gomega.HaveLen(1))
	g.Expect(events[0].Type).To(gomega.Equal("result"))
	g.Expect(events[0].Data["phase"]).To(gomega.Equal("Approved"))
}

type watchSignalClient struct {
	client.WithWatch
	watchEstablished chan struct{}
}

func (w *watchSignalClient) Watch(ctx context.Context,
	obj client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
	result, err := w.WithWatch.Watch(ctx, obj, opts...)
	select {
	case <-w.watchEstablished:
	default:
		close(w.watchEstablished)
	}
	return result, err
}

func TestStreamPhaseTransition(t *testing.T) {
	g := gomega.NewWithT(t)
	ar := pendingAgentRequest("transition-1", "default", "agent-1")
	s := newTestServerWithWatch(ar)

	watchReady := make(chan struct{})
	s.watchClient = &watchSignalClient{WithWatch: s.watchClient, watchEstablished: watchReady}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/agent-requests", nil)
	req.Header.Set("Accept", "text/event-stream")

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		s.streamAgentRequestPhase(rr, req, "transition-1", "default", map[string]string{"aip.io/agentIdentity": "agent-1"})
	}()

	<-watchReady

	var current v1alpha1.AgentRequest
	g.Expect(s.client.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "transition-1"}, &current)).To(gomega.Succeed())
	current.Status.Phase = v1alpha1.PhaseApproved
	g.Expect(s.client.Status().Update(context.Background(), &current)).To(gomega.Succeed())

	g.Eventually(doneCh, 5*time.Second).Should(gomega.BeClosed())

	events := parseSSEEvents(t, rr.Body.String())
	g.Expect(events).ToNot(gomega.BeEmpty())

	last := events[len(events)-1]
	g.Expect(last.Type).To(gomega.Equal("result"))
	g.Expect(last.Data["phase"]).To(gomega.Equal("Approved"))
}

func TestStreamTimeout(t *testing.T) {
	g := gomega.NewWithT(t)
	ar := pendingAgentRequest("timeout-1", "default", "agent-1")

	scheme := newTestScheme()
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ar).
		WithStatusSubresource(&v1alpha1.AgentRequest{}).
		Build()
	s := &Server{
		client:       fc,
		apiReader:    fc,
		watchClient:  fc,
		dedupWindow:  0,
		waitTimeout:  500 * time.Millisecond,
		roles:        newRoleConfig("", "", "", "", "", ""),
		authRequired: false,
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/agent-requests", nil)
	req.Header.Set("Accept", "text/event-stream")

	s.streamAgentRequestPhase(rr, req, "timeout-1", "default", map[string]string{})

	events := parseSSEEvents(t, rr.Body.String())
	g.Expect(events).ToNot(gomega.BeEmpty())

	last := events[len(events)-1]
	g.Expect(last.Type).To(gomega.Equal("error"))
	g.Expect(last.Data["error"]).To(gomega.ContainSubstring("timed out"))
}

func TestContentNegotiationSSE(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := newTestScheme()
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.AgentRequest{}).
		Build()
	s := &Server{
		client:       fc,
		apiReader:    fc,
		watchClient:  fc,
		dedupWindow:  0,
		waitTimeout:  500 * time.Millisecond,
		roles:        newRoleConfig("", "", "", "", "", ""),
		authRequired: false,
	}

	body := createAgentRequestBody{
		AgentIdentity: "agent-1",
		Action:        "restart",
		TargetURI:     "k8s://prod/default/deployment/cn-test",
		Reason:        "test",
		Namespace:     "default",
	}
	jsonBody, err := json.Marshal(body)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
	req.Header.Set("Accept", "text/event-stream")

	s.handleCreateAgentRequest(rr, req)

	g.Expect(rr.Header().Get("Content-Type")).To(gomega.Equal("text/event-stream"))
}

func TestContentNegotiationJSON(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := newTestScheme()
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.AgentRequest{}).
		Build()
	s := &Server{
		client:       fc,
		apiReader:    fc,
		watchClient:  fc,
		dedupWindow:  0,
		waitTimeout:  500 * time.Millisecond,
		roles:        newRoleConfig("", "", "", "", "", ""),
		authRequired: false,
	}

	body := createAgentRequestBody{
		AgentIdentity: "agent-1",
		Action:        "restart",
		TargetURI:     "k8s://prod/default/deployment/cn-json-test",
		Reason:        "test",
		Namespace:     "default",
	}
	jsonBody, err := json.Marshal(body)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))

	s.handleCreateAgentRequest(rr, req)

	g.Expect(rr.Header().Get("Content-Type")).To(gomega.Equal("application/json"))
}

func TestWatchEndpointRequiresAcceptHeader(t *testing.T) {
	g := gomega.NewWithT(t)
	ar := pendingAgentRequest("accept-1", "default", "agent-1")
	s := newTestServerWithWatch(ar)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/agent-requests/accept-1/watch", nil)
	req.SetPathValue("name", "accept-1")

	s.handleWatchAgentRequest(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusBadRequest))
}

func TestWatchEndpointNotFound(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServerWithWatch()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/agent-requests/nonexistent/watch", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.SetPathValue("name", "nonexistent")

	s.handleWatchAgentRequest(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusNotFound))
}

func TestSSEEventFormat(t *testing.T) {
	g := gomega.NewWithT(t)
	ar := approvedAgentRequest("fmt-1", "default", "agent-1")
	s := newTestServerWithWatch(ar)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/agent-requests/fmt-1/watch?namespace=default", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.SetPathValue("name", "fmt-1")

	s.handleWatchAgentRequest(rr, req)

	raw := rr.Body.String()
	g.Expect(raw).To(gomega.ContainSubstring("event: result\n"))
	g.Expect(raw).To(gomega.ContainSubstring("data: "))
	g.Expect(raw).To(gomega.ContainSubstring("\n\n"))
}
