/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMCPServer_Defaults(t *testing.T) {
	srv := MCPServer{}
	if srv.Status.DiscoveredToolCount != 0 {
		t.Errorf("expected 0 discovered tools, got %d", srv.Status.DiscoveredToolCount)
	}
	if srv.Status.Tools != nil {
		t.Errorf("expected nil tools, got %v", srv.Status.Tools)
	}
	if srv.Status.Conditions != nil {
		t.Errorf("expected nil conditions, got %v", srv.Status.Conditions)
	}
}

func TestMCPServer_SpecURL(t *testing.T) {
	srv := MCPServer{
		Spec: MCPServerSpec{
			URL: "http://github-mcp:80",
		},
	}
	if srv.Spec.URL != "http://github-mcp:80" {
		t.Errorf("URL = %q, want %q", srv.Spec.URL, "http://github-mcp:80")
	}
}

func TestMCPServer_SpecWithSecretRef(t *testing.T) {
	srv := MCPServer{
		Spec: MCPServerSpec{
			URL: "http://github-mcp:80",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "my-token"},
				Key:                  "token",
			},
		},
	}
	if srv.Spec.BearerTokenSecretRef.Name != "my-token" {
		t.Errorf("SecretRef.Name = %q, want %q", srv.Spec.BearerTokenSecretRef.Name, "my-token")
	}
	if srv.Spec.BearerTokenSecretRef.Key != "token" {
		t.Errorf("SecretRef.Key = %q, want %q", srv.Spec.BearerTokenSecretRef.Key, "token")
	}
}

func TestMCPServer_SpecReadOnlyTools(t *testing.T) {
	srv := MCPServer{
		Spec: MCPServerSpec{
			URL:           "http://mcp:80",
			ReadOnlyTools: []string{"get_data", "list_data"},
		},
	}
	if len(srv.Spec.ReadOnlyTools) != 2 {
		t.Fatalf("expected 2 read-only tools, got %d", len(srv.Spec.ReadOnlyTools))
	}
	if srv.Spec.ReadOnlyTools[0] != "get_data" {
		t.Errorf("ReadOnlyTools[0] = %q, want %q", srv.Spec.ReadOnlyTools[0], "get_data")
	}
	if srv.Spec.ReadOnlyTools[1] != "list_data" {
		t.Errorf("ReadOnlyTools[1] = %q, want %q", srv.Spec.ReadOnlyTools[1], "list_data")
	}
}

func TestMCPServer_StatusWithTools(t *testing.T) {
	srv := MCPServer{
		Status: MCPServerStatus{
			DiscoveredToolCount: 2,
			Tools: []MCPServerTool{
				{Name: "create_pull_request", ReadOnly: false},
				{Name: "get_file_contents", ReadOnly: true},
			},
		},
	}
	if srv.Status.DiscoveredToolCount != 2 {
		t.Errorf("DiscoveredToolCount = %d, want %d", srv.Status.DiscoveredToolCount, 2)
	}
	if len(srv.Status.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(srv.Status.Tools))
	}
	if srv.Status.Tools[0].ReadOnly != false {
		t.Errorf("Tools[0].ReadOnly should be false")
	}
	if srv.Status.Tools[1].ReadOnly != true {
		t.Errorf("Tools[1].ReadOnly should be true")
	}
}

func TestMCPServer_JSONRoundTrip(t *testing.T) {
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	original := MCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "github",
		},
		Spec: MCPServerSpec{
			URL: "http://github-mcp:80",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "aip-github-token"},
				Key:                  "token",
			},
			ReadOnlyTools: []string{"get_file_contents", "list_pull_requests"},
		},
		Status: MCPServerStatus{
			LastSyncTime:        &now,
			ObservedGeneration:  1,
			DiscoveredToolCount: 3,
			Conditions: []metav1.Condition{
				{
					Type:               "Synced",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
					Reason:             "DiscoverySucceeded",
					Message:            "Discovered 3 tools from upstream",
				},
			},
			Tools: []MCPServerTool{
				{Name: "create_pull_request", ReadOnly: false},
				{Name: "get_file_contents", ReadOnly: true},
				{Name: "list_pull_requests", ReadOnly: true},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("failed to marshal MCPServer: %v", err)
	}

	var decoded MCPServer
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal MCPServer: %v", err)
	}

	if decoded.Name != "github" {
		t.Errorf("Name = %q, want %q", decoded.Name, "github")
	}
	if decoded.Spec.URL != "http://github-mcp:80" {
		t.Errorf("URL = %q, want %q", decoded.Spec.URL, "http://github-mcp:80")
	}
	if decoded.Spec.BearerTokenSecretRef.Name != "aip-github-token" {
		t.Errorf("SecretRef.Name = %q", decoded.Spec.BearerTokenSecretRef.Name)
	}
	if len(decoded.Spec.ReadOnlyTools) != 2 {
		t.Errorf("expected 2 read-only tools, got %d", len(decoded.Spec.ReadOnlyTools))
	}
	if decoded.Status.ObservedGeneration != 1 {
		t.Errorf("ObservedGeneration = %d, want %d", decoded.Status.ObservedGeneration, 1)
	}
	if decoded.Status.DiscoveredToolCount != 3 {
		t.Errorf("DiscoveredToolCount = %d, want %d", decoded.Status.DiscoveredToolCount, 3)
	}
	if len(decoded.Status.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(decoded.Status.Tools))
	}
	if decoded.Status.Tools[0].Name != "create_pull_request" {
		t.Errorf("Tools[0].Name = %q", decoded.Status.Tools[0].Name)
	}
	if decoded.Status.Tools[0].ReadOnly != false {
		t.Errorf("Tools[0].ReadOnly should be false")
	}
	if len(decoded.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(decoded.Status.Conditions))
	}
	if decoded.Status.Conditions[0].Reason != "DiscoverySucceeded" {
		t.Errorf("Condition.Reason = %q", decoded.Status.Conditions[0].Reason)
	}
}

func TestMCPServerTool(t *testing.T) {
	tool := MCPServerTool{
		Name:     "create_pull_request",
		ReadOnly: false,
	}
	if tool.Name != "create_pull_request" {
		t.Errorf("Name = %q", tool.Name)
	}
	if tool.ReadOnly {
		t.Errorf("ReadOnly should be false")
	}

	readTool := MCPServerTool{Name: "get_file_contents", ReadOnly: true}
	if !readTool.ReadOnly {
		t.Errorf("ReadOnly should be true")
	}
}

func TestMCPServer_EmptyList(t *testing.T) {
	list := MCPServerList{}
	if len(list.Items) != 0 {
		t.Errorf("expected empty list, got %d items", len(list.Items))
	}
}

func TestMCPServer_ListWithItems(t *testing.T) {
	list := MCPServerList{
		Items: []MCPServer{
			{ObjectMeta: metav1.ObjectMeta{Name: "github"}, Spec: MCPServerSpec{URL: "http://gh:80"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "jira"}, Spec: MCPServerSpec{URL: "http://jira:80"}},
		},
	}
	if len(list.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(list.Items))
	}
	if list.Items[0].Name != "github" {
		t.Errorf("Items[0].Name = %q", list.Items[0].Name)
	}
	if list.Items[1].Spec.URL != "http://jira:80" {
		t.Errorf("Items[1].URL = %q", list.Items[1].Spec.URL)
	}
}

func TestMCPServer_SecretKeySelectorRoundTrip(t *testing.T) {
	original := MCPServer{
		Spec: MCPServerSpec{
			URL: "http://mcp:80",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
				Key:                  "my-key",
				Optional:             boolPtr(true),
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded MCPServer
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Spec.BearerTokenSecretRef.Name != "my-secret" {
		t.Errorf("Name = %q", decoded.Spec.BearerTokenSecretRef.Name)
	}
	if decoded.Spec.BearerTokenSecretRef.Key != "my-key" {
		t.Errorf("Key = %q", decoded.Spec.BearerTokenSecretRef.Key)
	}
	if decoded.Spec.BearerTokenSecretRef.Optional == nil || !*decoded.Spec.BearerTokenSecretRef.Optional {
		t.Errorf("Optional should be true")
	}
}

func boolPtr(v bool) *bool { return &v }

func TestMCPServer_NoSecretRef(t *testing.T) {
	srv := MCPServer{
		Spec: MCPServerSpec{
			URL: "http://public-mcp:80",
		},
	}
	if srv.Spec.BearerTokenSecretRef != nil {
		t.Error("BearerTokenSecretRef should be nil when not set")
	}
}
