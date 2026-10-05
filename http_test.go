package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const modernHTTPVersion = "2026-07-28"

func testHTTPHandler(t *testing.T) (http.Handler, *mcp.Server) {
	t.Helper()
	srv := newServer(&Config{}, nil, t.TempDir())
	m := mcp.NewServer(&mcp.Implementation{Name: "your-mail-mcp", Version: "test"}, nil)
	srv.registerTools(m)
	o := testOAuth(t)
	o.access["http-test-token"] = time.Now().Add(time.Hour)
	return newHTTPHandler(o, m, srv), m
}

func httpRPCRequest(t *testing.T, endpoint, version, method string, params map[string]any) *http.Request {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	if version == modernHTTPVersion {
		params["_meta"] = map[string]any{
			mcp.MetaKeyProtocolVersion:    version,
			mcp.MetaKeyClientCapabilities: map[string]any{},
		}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer http-test-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", version)
	if version == modernHTTPVersion {
		req.Header.Set("Mcp-Method", method)
		if name, ok := params["name"].(string); ok {
			req.Header.Set("Mcp-Name", name)
		}
	}
	return req
}

func httpRPCResult(t *testing.T, handler http.Handler, req *http.Request, result any) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Mcp-Session-Id"); got != "" {
		t.Fatalf("unexpected MCP session ID %q", got)
	}
	payload := rec.Body.String()
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		payload = ""
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if strings.HasPrefix(line, "data: ") {
				payload = strings.TrimPrefix(line, "data: ")
				break
			}
		}
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &rpc); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if len(rpc.Error) != 0 || len(rpc.Result) == 0 {
		t.Fatalf("RPC response = %s", payload)
	}
	if err := json.Unmarshal(rpc.Result, result); err != nil {
		t.Fatalf("decode result %s: %v", rpc.Result, err)
	}
}

func TestHTTPModernDiscoveryAndToolsWithoutInitialization(t *testing.T) {
	handler, _ := testHTTPHandler(t)
	var discovery mcp.DiscoverResult
	httpRPCResult(t, handler, httpRPCRequest(t, "/mcp", modernHTTPVersion, "server/discover", nil), &discovery)
	if !containsString(discovery.SupportedVersions, modernHTTPVersion) {
		t.Fatalf("discovery versions = %v, want %s", discovery.SupportedVersions, modernHTTPVersion)
	}
	if discovery.Capabilities.Tools == nil {
		t.Fatal("discovery does not advertise tools")
	}
	checkHTTPTools(t, handler, modernHTTPVersion)
}

func checkHTTPTools(t *testing.T, handler http.Handler, version string) {
	t.Helper()
	var tools struct {
		ResultType string     `json:"resultType"`
		Tools      []mcp.Tool `json:"tools"`
	}
	httpRPCResult(t, handler, httpRPCRequest(t, "/mcp", version, "tools/list", nil), &tools)
	if len(tools.Tools) != 11 {
		t.Fatalf("got %d tools, want 11", len(tools.Tools))
	}
	if version == modernHTTPVersion && tools.ResultType != "complete" {
		t.Errorf("tools/list resultType = %q, want complete", tools.ResultType)
	}
	var call struct {
		ResultType string `json:"resultType"`
		IsError    bool   `json:"isError"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	httpRPCResult(t, handler, httpRPCRequest(t, "/mcp", version, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{}}), &call)
	if call.IsError || len(call.Content) != 1 || call.Content[0].Type != "text" || !strings.Contains(call.Content[0].Text, "no accounts configured") {
		t.Fatalf("status result = %+v", call)
	}
	if version == modernHTTPVersion && call.ResultType != "complete" {
		t.Errorf("tools/call resultType = %q, want complete", call.ResultType)
	}
}

func TestHTTPLegacyInitializationAndTools(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"} {
		t.Run(version, func(t *testing.T) {
			handler, _ := testHTTPHandler(t)
			var initialized mcp.InitializeResult
			httpRPCResult(t, handler, httpRPCRequest(t, "/mcp", version, "initialize", map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "legacy-test", "version": "test"},
			}), &initialized)
			if initialized.ProtocolVersion != version {
				t.Fatalf("negotiated version = %q, want %q", initialized.ProtocolVersion, version)
			}
			checkHTTPTools(t, handler, version)
		})
	}
}

func TestHTTPModernAuthenticationAndHeaderValidation(t *testing.T) {
	handler, _ := testHTTPHandler(t)
	for _, tc := range []struct {
		name, header, value string
		status              int
	}{
		{"missing bearer", "Authorization", "", http.StatusUnauthorized},
		{"invalid bearer", "Authorization", "Bearer invalid", http.StatusUnauthorized},
		{"missing method", "Mcp-Method", "", http.StatusBadRequest},
		{"mismatched method", "Mcp-Method", "tools/call", http.StatusBadRequest},
		{"mismatched version", "Mcp-Protocol-Version", "2025-11-25", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httpRPCRequest(t, "/mcp", modernHTTPVersion, "server/discover", nil)
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
		})
	}
}

func TestHTTPModernRequestCancellationReachesTool(t *testing.T) {
	handler, m := testHTTPHandler(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	mcp.AddTool(m, &mcp.Tool{Name: "wait", Description: "Wait for cancellation"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			close(started)
			select {
			case <-ctx.Done():
				close(cancelled)
				return nil, nil, ctx.Err()
			case <-time.After(10 * time.Second):
				return nil, nil, context.DeadlineExceeded
			}
		})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httpRPCRequest(t, ts.URL+"/mcp", modernHTTPVersion, "tools/call", map[string]any{"name": "wait", "arguments": map[string]any{}}).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := ts.Client().Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP cancellation did not reach the tool")
	}
	<-done
}
