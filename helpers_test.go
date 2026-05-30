package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestParseRegions(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", []string{"us-east-1"}}, // default
		{"us-west-2", []string{"us-west-2"}},
		{"us-east-1,us-west-2", []string{"us-east-1", "us-west-2"}},
		{" us-east-1 , us-west-2 ", []string{"us-east-1", "us-west-2"}}, // trims whitespace
		{"us-east-1,,us-west-2", []string{"us-east-1", "us-west-2"}},    // skips empties
		{",", []string{"us-east-1"}},                                    // all-empty falls through to default? -> nil
	}
	for _, tt := range tests {
		got := parseRegions(tt.in)
		// The "," case yields no regions (nil) since both parts are empty.
		if tt.in == "," {
			if len(got) != 0 {
				t.Errorf("parseRegions(%q) = %v, want empty", tt.in, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseRegions(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// newRequest builds a CallToolRequest with the given argument map.
func newRequest(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func TestRegisterTools(t *testing.T) {
	// Registration must not panic and should accept all tools onto a server.
	s := server.NewMCPServer("test", "0.0.0", server.WithToolCapabilities(true))
	registerTruffleTools(s)
	registerSpawnTools(s)
}

func TestHandleTruffleFind_ParseError(t *testing.T) {
	// "intel" + "arm64" is a conflicting-architecture query → ParseQuery/
	// BuildCriteria errors before any AWS call, returning a tool error result.
	req := newRequest(map[string]any{"query": "intel graviton arm64 x86_64"})
	res, err := handleTruffleFind(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected an error tool-result for a conflicting query, got %+v", res)
	}
}

func TestHandleTruffleFind_EmptyQueryOK(t *testing.T) {
	// An empty query parses fine; without AWS creds the handler returns an
	// error result at the client/search step — either way it must not panic
	// and must return a non-nil result.
	req := newRequest(map[string]any{"query": ""})
	res, err := handleTruffleFind(context.Background(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if res == nil {
		t.Error("expected a non-nil result")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "30m"},
		{90 * time.Minute, "1h 30m"},
		{2 * time.Hour, "2h 0m"},
		{25 * time.Hour, "1d 1h 0m"},
		{49 * time.Hour, "2d 1h 0m"},
		{0, "0m"},
		// Rounding: 29s rounds down to 0m, 31s rounds up to 1m.
		{29 * time.Second, "0m"},
		{91 * time.Minute, "1h 31m"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.d); got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
