package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClaimTask_200ReturnsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/tasks/claim" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"research_task","research_run_id":42,"project_id":"p1","llm":{"base_url":"x","model":"m"}}`))
	}))
	defer srv.Close()

	env, ok, err := claimTask(context.Background(), AgentConfig{Server: srv.URL, Token: "tok123"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok || env == nil {
		t.Fatalf("expected a claimed task, got ok=%v env=%v", ok, env)
	}
	if env.ResearchRunID != 42 || env.ProjectID != "p1" {
		t.Fatalf("envelope round-trip: %+v", env)
	}
}

func TestClaimTask_204EmptyQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	env, ok, err := claimTask(context.Background(), AgentConfig{Server: srv.URL, Token: "t"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok || env != nil {
		t.Fatalf("expected empty queue, got ok=%v env=%v", ok, env)
	}
}

func TestClaimTask_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, ok, err := claimTask(context.Background(), AgentConfig{Server: srv.URL, Token: "t"}); err == nil || ok {
		t.Fatalf("expected error on 500, got ok=%v err=%v", ok, err)
	}
}
