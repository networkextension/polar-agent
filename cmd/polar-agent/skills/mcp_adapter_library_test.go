package skills

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLibraryAdapterUnavailableWhenNoConfig(t *testing.T) {
	// Save + restore the package-level config so tests don't leak.
	server, token := libraryCfg.snapshot()
	defer SetLibraryAdapterConfig(server, token)

	SetLibraryAdapterConfig("", "")
	f, ok := mcpAdapterFactories["library"]
	if !ok {
		t.Fatal("library adapter not registered")
	}
	_, err := f()
	if err == nil {
		t.Fatal("expected errAdapterUnavailable when no config; got nil")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("want 'unavailable' in error, got %v", err)
	}
}

func TestLibraryAdapterToolList(t *testing.T) {
	server, token := libraryCfg.snapshot()
	defer SetLibraryAdapterConfig(server, token)
	SetLibraryAdapterConfig("http://test", "tok")
	f := mcpAdapterFactories["library"]
	a, err := f()
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	tools := a.Tools()
	wanted := map[string]bool{
		"library.devices.list":              true,
		"library.devices.get":               true,
		"library.devices.recent_seen":       true,
		"library.firmwares.list":            true,
		"library.firmwares.get":             true,
		"library.firmwares.matching":        true,
		"library.functions.lookup_by_address": true,
		"library.functions.lookup_by_symbol":  true,
		"library.functions.search":          true,
	}
	for _, td := range tools {
		if !wanted[td.Name] {
			t.Errorf("unexpected tool %q", td.Name)
		}
		delete(wanted, td.Name)
		if td.Description == "" {
			t.Errorf("tool %q has empty description", td.Name)
		}
	}
	for missing := range wanted {
		t.Errorf("missing expected tool: %q", missing)
	}
}

// Wire test — spin up a fake dock + verify a couple of representative
// tool calls forward + return the expected shape.
func TestLibraryAdapterRoundTrip(t *testing.T) {
	captured := struct {
		path string
		auth string
	}{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path + "?" + r.URL.RawQuery
		captured.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/library/devices":
			_, _ = w.Write([]byte(`{"devices":[{"id":1,"cpid":33025,"chip_name":"M1"}]}`))
		case "/api/library/functions/lookup-by-address":
			_, _ = w.Write([]byte(`{"function":{"id":42,"firmware_id":1,"address":2147487796,"symbol":"iboot_main","prototype":"void iboot_main(void)"}}`))
		case "/api/library/functions/search":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"q required"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer mock.Close()

	server, token := libraryCfg.snapshot()
	defer SetLibraryAdapterConfig(server, token)
	SetLibraryAdapterConfig(mock.URL, "test-token")

	a, err := mcpAdapterFactories["library"]()
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	// devices.list with no args — base path with empty query
	got, err := a.Call("library.devices.list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("devices.list: %v", err)
	}
	if captured.auth != "Bearer test-token" {
		t.Errorf("auth header: got %q want 'Bearer test-token'", captured.auth)
	}
	if !strings.HasPrefix(captured.path, "/api/library/devices") {
		t.Errorf("path: got %q", captured.path)
	}
	// The structured top-level key should bleed through
	if _, ok := got["devices"]; !ok {
		t.Errorf("expected top-level 'devices' key in result; got %v", got)
	}

	// lookup_by_address — most-used. Verify the address param flows
	// through verbatim (no decimal-conversion in the adapter; dock
	// handles hex parsing).
	got, err = a.Call("library.functions.lookup_by_address",
		json.RawMessage(`{"firmware_id":1,"address":"0x80001234"}`))
	if err != nil {
		t.Fatalf("lookup_by_address: %v", err)
	}
	if !strings.Contains(captured.path, "address=0x80001234") {
		t.Errorf("expected address=0x80001234 in query; got %q", captured.path)
	}
	fnObj, _ := got["function"].(map[string]any)
	if fnObj["symbol"] != "iboot_main" {
		t.Errorf("expected function.symbol=iboot_main; got %v", fnObj)
	}

	// Error propagation — 400 should land in the result with isError
	got, err = a.Call("library.functions.search", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("search Call: unexpected go err %v", err)
	}
	if got["isError"] != true {
		t.Errorf("expected isError=true on 400; got %v", got)
	}
}

func TestLibraryAdapterUnknownTool(t *testing.T) {
	server, token := libraryCfg.snapshot()
	defer SetLibraryAdapterConfig(server, token)
	SetLibraryAdapterConfig("http://test", "tok")
	a, _ := mcpAdapterFactories["library"]()
	_, err := a.Call("library.does.not.exist", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestLibraryAdapterMissingRequiredArg(t *testing.T) {
	server, token := libraryCfg.snapshot()
	defer SetLibraryAdapterConfig(server, token)
	SetLibraryAdapterConfig("http://test", "tok")
	a, _ := mcpAdapterFactories["library"]()

	cases := []struct {
		tool string
		args string
	}{
		{"library.devices.get", `{}`},
		{"library.firmwares.get", `{"id":0}`},
		{"library.firmwares.matching", `{"bdid":10}`},
		{"library.functions.lookup_by_address", `{"firmware_id":1}`},
		{"library.functions.lookup_by_symbol", `{"firmware_id":1}`},
		{"library.functions.search", `{}`},
	}
	for _, tc := range cases {
		_, err := a.Call(tc.tool, json.RawMessage(tc.args))
		if err == nil {
			t.Errorf("%s: expected required-arg error, got nil", tc.tool)
		}
	}
}
