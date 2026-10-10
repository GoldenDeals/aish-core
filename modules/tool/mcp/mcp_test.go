package mcp_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/tool/mcp"
	"github.com/GoldenDeals/aish-core/test"
)

type echoIn struct {
	Text string `json:"text"`
}

func echo(_ context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "echo " + in.Text}}}, nil, nil
}

func startServer(t *testing.T) (*sdk.Server, sdk.Transport) {
	t.Helper()

	srv := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "echo", Description: "Echo", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, echo)
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}

	return srv, ct
}

func session(t *testing.T, tr sdk.Transport) *test.Session {
	t.Helper()

	return test.NewSession(t, aish.Instance{Name: "tool.mcp/fake", Module: &mcp.MCP{Server: "fake", Transport: tr}})
}

func toolNames(t *testing.T, s *test.Session) []string {
	t.Helper()

	if err := s.Emit(events.LLMRequest{}); err != nil {
		t.Fatal(err)
	}
	req := s.Last().Events[0].Event.(events.LLMRequest)
	names := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}

	return names
}

func TestToolsAndCall(t *testing.T) {
	t.Parallel()

	_, tr := startServer(t)
	s := session(t, tr)

	if got := toolNames(t, s); len(got) != 1 || got[0] != "mcp__fake__echo" {
		t.Fatalf("tools = %v", got)
	}

	err := s.Emit(events.ToolCall{ID: "c1", Name: "mcp__fake__echo", Input: []byte(`{"text":"hi"}`)})
	if err != nil && !errors.Is(err, aish.ErrUnhandled) {
		t.Fatal(err)
	}
	var call events.ToolCall
	var res events.ToolResult
	for _, e := range s.Last().Events {
		switch ev := e.Event.(type) {
		case events.ToolCall:
			call = ev
		case events.ToolResult:
			res = ev
		}
	}
	if !call.Effects.ReadOnly || call.Effects.Access[0].Resource != "mcp:fake/echo" || call.Effects.Access[0].Action != "mcp.call" {
		t.Errorf("effects = %+v", call.Effects)
	}
	if res.IsError || len(res.Content) != 1 || res.Content[0].Text != "echo hi" {
		t.Errorf("result = %+v", res)
	}
}

func TestListChanged(t *testing.T) {
	t.Parallel()

	srv, tr := startServer(t)
	s := session(t, tr)
	sdk.AddTool(srv, &sdk.Tool{Name: "second", Description: "Second"}, echo)

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if strings.Join(toolNames(t, s), ",") == "mcp__fake__echo,mcp__fake__second" ||
			strings.Join(toolNames(t, s), ",") == "mcp__fake__second,mcp__fake__echo" {
			return
		}
	}
	t.Fatalf("tools = %v", toolNames(t, s))
}

func TestValidate(t *testing.T) {
	t.Parallel()

	for name, m := range map[string]*mcp.MCP{
		"no transport": {Server: "a"},
		"both":         {Server: "a", Command: []string{"x"}, URL: "http://x"},
		"underscore":   {Server: "a_b", URL: "http://x"},
	} {
		if m.Validate() == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := (&mcp.MCP{Server: "git-hub", URL: "http://x"}).Validate(); err != nil {
		t.Error(err)
	}
}
