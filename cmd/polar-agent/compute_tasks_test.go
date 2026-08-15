package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClaimComputeTask_204(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/compute-tasks/claim" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var body struct{ Skills []string `json:"skills"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Skills) != 1 || body.Skills[0] != "cloud.vm" {
			t.Errorf("skills = %v", body.Skills)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	task, err := claimComputeTask(context.Background(), AgentConfig{Server: srv.URL, Token: "tok"}, []string{"cloud.vm"})
	if err != nil || task != nil {
		t.Fatalf("want (nil,nil), got (%v,%v)", task, err)
	}
}

func TestClaimComputeTask_200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ct_1","workspace_id":"ws","skill":"cloud.vm","input":{"op":"status","vm_id":"a"},"constraints":{"host_id":"h1"},"priority":3,"status":"running"}`)
	}))
	defer srv.Close()
	task, err := claimComputeTask(context.Background(), AgentConfig{Server: srv.URL, Token: "tok"}, []string{"cloud.vm"})
	if err != nil || task == nil {
		t.Fatalf("err=%v task=%v", err, task)
	}
	if task.ID != "ct_1" || task.Skill != "cloud.vm" || task.Priority != 3 {
		t.Fatalf("bad task: %+v", task)
	}
	var in map[string]any
	if err := json.Unmarshal(task.Input, &in); err != nil || in["op"] != "status" {
		t.Fatalf("input not preserved: %s", task.Input)
	}
}

func TestCompleteComputeTask_Body(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/compute-tasks/complete/ct_9" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	err := completeComputeTask(context.Background(), AgentConfig{Server: srv.URL, Token: "tok"}, "ct_9",
		computeResult{OK: false, Error: "boom", Output: map[string]any{"vm_id": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if got["ok"] != false || got["error"] != "boom" {
		t.Fatalf("body = %v", got)
	}
	if out, _ := got["output"].(map[string]any); out["vm_id"] != "a" {
		t.Fatalf("output = %v", got["output"])
	}
}

func TestCompleteComputeTask_NilOutputBecomesObject(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := completeComputeTask(context.Background(), AgentConfig{Server: srv.URL}, "x", computeResult{OK: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["output"].(map[string]any); !ok {
		t.Fatalf("output should be an object, got %T", got["output"])
	}
}

func TestComputeHandlerRegistry(t *testing.T) {
	computeMu.Lock()
	saved := computeHandlers
	computeHandlers = map[string]computeHandler{}
	computeMu.Unlock()
	defer func() { computeMu.Lock(); computeHandlers = saved; computeMu.Unlock() }()
	if len(computeSkills()) != 0 {
		t.Fatal("expected empty")
	}
	registerComputeHandler("z.skill", func(context.Context, AgentConfig, computeTask) (any, error) { return nil, nil })
	registerComputeHandler("a.skill", func(context.Context, AgentConfig, computeTask) (any, error) { return nil, nil })
	sk := computeSkills()
	if len(sk) != 2 || sk[0] != "a.skill" || sk[1] != "z.skill" {
		t.Fatalf("skills = %v", sk)
	}
	if lookupComputeHandler("nope") != nil {
		t.Fatal("unexpected handler")
	}
}
