package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	agentID := flag.String("agent-id", "", "ID of the agent (required)")
	target := flag.String("target", "", "Target URI (required)")
	gateway := flag.String("gateway", "http://localhost:8080", "Gateway URL")
	namespace := flag.String("namespace", "default", "Kubernetes namespace")
	flag.Parse()

	if *agentID == "" || *target == "" {
		flag.Usage()
		os.Exit(1)
	}

	log.SetFlags(0)
	logger := log.New(os.Stdout, fmt.Sprintf("[%s] ", *agentID), log.LstdFlags)

	// 1. Submit AgentRequest
	body := map[string]string{
		"agentIdentity": *agentID,
		"action":        "scale",
		"targetURI":     *target,
		"reason":        "autonomous scale operation",
		"namespace":     *namespace,
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(*gateway+"/agent-requests", "application/json", bytes.NewBuffer(b))
	if err != nil {
		logger.Fatalf("Failed to connect to gateway: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var errResp map[string]string
		json.NewDecoder(resp.Body).Decode(&errResp)
		logger.Fatalf("Error submitting request: %s", errResp["error"])
	}

	var arResp struct {
		Name   string `json:"name"`
		Phase  string `json:"phase"`
		Denial struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"denial"`
	}
	json.NewDecoder(resp.Body).Decode(&arResp)

	logger.Printf("→ Submitted AgentRequest: %s", arResp.Name)

	if arResp.Phase == "Denied" {
		logger.Fatalf("✗ Denied — code: %s, message: %s", arResp.Denial.Code, arResp.Denial.Message)
	}

	if arResp.Phase == "Approved" {
		logger.Printf("✓ Approved — acquiring OpsLock, signalling Executing...")

		// 4. Signal Executing
		execResp, err := http.Post(fmt.Sprintf("%s/agent-requests/%s/executing?namespace=%s", *gateway, arResp.Name, *namespace), "application/json", nil)
		if err != nil {
			logger.Fatalf("Failed to signal executing: %v", err)
		}
		execResp.Body.Close()

		// 5. Simulate work
		time.Sleep(10 * time.Second)

		// 6. Signal Completed
		compResp, err := http.Post(fmt.Sprintf("%s/agent-requests/%s/completed?namespace=%s", *gateway, arResp.Name, *namespace), "application/json", nil)
		if err != nil {
			logger.Fatalf("Failed to signal completed: %v", err)
		}
		compResp.Body.Close()

		logger.Printf("✓ Completed successfully")
	} else if arResp.Phase == "Completed" {
		// This might happen if the gateway returns once it's already completed (unlikely in this flow but possible)
		logger.Printf("✓ Completed successfully")
	} else {
		logger.Fatalf("Unexpected phase: %s", arResp.Phase)
	}
}
