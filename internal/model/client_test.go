package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "", false, 10*time.Second)
}

func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

func TestStreamTextAndToolCalls(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		sse(w,
			`{"choices":[{"delta":{"content":"Hello "}}]}`,
			`{"choices":[{"delta":{"content":"world"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"plan.md\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	})
	var streamed strings.Builder
	req := ChatRequest{Model: "m", Messages: []Message{TextMessage("user", "hi")}, Tools: []Tool{NewTool("read_file", "", nil, nil)}}
	reply, err := c.Chat(context.Background(), req, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "Hello world" {
		t.Fatalf("content: %q", reply.Content)
	}
	if streamed.String() != "Hello world" {
		t.Fatalf("streamed: %q", streamed.String())
	}
	if reply.FinishReason != "tool_calls" {
		t.Fatalf("finish: %q", reply.FinishReason)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("tool calls: %+v", reply.ToolCalls)
	}
	tc := reply.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read_file" || tc.Function.Arguments != `{"path":"plan.md"}` {
		t.Fatalf("merged tool call wrong: %+v", tc)
	}
}

func TestToolCallWithoutIndex(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			`{"choices":[{"delta":{"tool_calls":[{"id":"x1","function":{"name":"run_command","arguments":"{\"command\": \"echo"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":" hi\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	})
	reply, err := c.Chat(context.Background(), ChatRequest{Model: "m", Messages: nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].Function.Arguments != `{"command": "echo hi"}` {
		t.Fatalf("got %+v", reply.ToolCalls)
	}
}

func TestHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad api key", http.StatusUnauthorized)
	})
	_, err := c.Chat(context.Background(), ChatRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
}

func TestErrorChunk(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, `{"error":{"message":"context too long"}}`)
	})
	_, err := c.Chat(context.Background(), ChatRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "context too long") {
		t.Fatalf("got %v", err)
	}
}

func TestEmptyResponseError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	})
	_, err := c.Chat(context.Background(), ChatRequest{}, nil)
	if err == nil {
		t.Fatal("empty stream must error")
	}
}

func TestAuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		sse(w, `{"choices":[{"delta":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "secret-key", false, time.Second)
	if _, err := c.Chat(context.Background(), ChatRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("auth header: %q", gotAuth)
	}
}
