package main

import (
	"net/http"
	"net/url"
	"testing"
)

// buildProxyFunc has three rules. Cover each.

func TestBuildProxyFuncEmptyProxyURLAlwaysDirect(t *testing.T) {
	pf := buildProxyFunc("", "https://dock.example.com")
	for _, target := range []string{"https://api.x.ai/v1", "https://api.openai.com/v1", "https://example.com"} {
		req := mustReq(t, target)
		got, err := pf(req)
		if err != nil {
			t.Fatalf("err for %s: %v", target, err)
		}
		if got != nil {
			t.Errorf("empty proxyURL must always direct; got %v for %s", got, target)
		}
	}
}

func TestBuildProxyFuncBypassesDockHost(t *testing.T) {
	pf := buildProxyFunc("http://192.168.11.35:10082", "https://zen.4950.store")
	dockReq := mustReq(t, "https://zen.4950.store/api/research/runs/1/start")
	got, err := pf(dockReq)
	if err != nil {
		t.Fatalf("dock req err: %v", err)
	}
	if got != nil {
		t.Errorf("dock host must bypass proxy, got %v", got)
	}
	apiReq := mustReq(t, "https://api.x.ai/v1/responses")
	got, err = pf(apiReq)
	if err != nil {
		t.Fatalf("api req err: %v", err)
	}
	if got == nil || got.Host != "192.168.11.35:10082" {
		t.Errorf("api req should use proxy 192.168.11.35:10082, got %v", got)
	}
}

func TestBuildProxyFuncBypassesPrivateAddrs(t *testing.T) {
	pf := buildProxyFunc("http://proxy.example.com:8080", "https://dock.example.com")
	for _, target := range []string{
		"http://localhost:8080/x",
		"http://127.0.0.1/x",
		"http://10.1.2.3/x",
		"http://192.168.1.4/x",
		"http://172.16.5.6/x",
	} {
		got, err := pf(mustReq(t, target))
		if err != nil {
			t.Fatalf("err for %s: %v", target, err)
		}
		if got != nil {
			t.Errorf("private addr must bypass proxy; got %v for %s", got, target)
		}
	}
}

func TestBuildProxyFuncRoutesPublicHostThroughProxy(t *testing.T) {
	pf := buildProxyFunc("http://proxy.local:3128", "")
	got, err := pf(mustReq(t, "https://api.deepseek.com/v1/chat/completions"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got == nil {
		t.Fatalf("public api should be proxied, got nil")
	}
	if got.Host != "proxy.local:3128" {
		t.Errorf("expected proxy.local:3128, got %s", got.Host)
	}
}

func TestBuildProxyFuncBadProxyURLFallsBackToDirect(t *testing.T) {
	// Garbage proxy URL → fall back to direct so a misconfig doesn't
	// black-hole all LLM traffic.
	pf := buildProxyFunc("not://valid", "")
	got, err := pf(mustReq(t, "https://api.openai.com/v1"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("bad proxy URL must fall back to direct, got %v", got)
	}
}

func mustReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return &http.Request{URL: u}
}
