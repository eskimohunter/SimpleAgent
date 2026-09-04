// Package mock provides a scripted, offline stand-in for the model server
// (activated with --mock). It speaks just enough of the OpenAI SSE chat
// protocol for the real Client to work against it, so the whole harness -
// approvals, sandbox, audit trail - can be exercised end to end without a
// model or an API key.
package mock

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"simpleagent/internal/model"
)

// Brain is the pluggable "model": given the conversation so far it decides
// what to reply. nil means use DefaultBrain. Tests inject their own brain to
// script specific tool-call sequences.
type Brain func(messages []model.Message) model.Reply

// Server is a minimal HTTP server exposing /v1/chat/completions.
type Server struct {
	brain Brain
	srv   *http.Server
	ln    net.Listener
}

// New creates a server with the given brain (nil -> DefaultBrain).
func New(brain Brain) *Server {
	return &Server{brain: brain}
}

// Addr returns the listener address ("127.0.0.1:<port>"), used by main.go
// to point the client at this server.
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// Start binds the server to a random free port on loopback only - a model
// server that is local by construction, never reachable from the network.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln)
	return nil
}

// Close shuts the server down.
func (s *Server) Close() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

// Brain returns the configured brain, or DefaultBrain.
func (s *Server) Brain() Brain {
	if s.brain != nil {
		return s.brain
	}
	return DefaultBrain
}

// handleChat serves one chat request the way a real server would: decode
// the request, compute the reply, then stream it back as an SSE response
// with word-sized content deltas, tool-call deltas and a finish_reason,
// terminated by the "[DONE]" marker.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req model.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reply := s.Brain()(req.Messages)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, ok := w.(http.Flusher)
	if !ok {
		return
	}

	// writeChunk emits one SSE frame: "data: <json>\n\n" then flush so the
	// client sees it immediately (streaming only works with flushes).
	writeChunk := func(obj map[string]any) error {
		b, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}

	// Emit the reply's text one word at a time with a tiny sleep, imitating
	// the pacing of a real model so streaming behavior is exercised too.
	words := strings.Fields(reply.Content)
	for _, word := range words {
		delta := word
		if !strings.HasSuffix(delta, "\n") && len(words) > 1 {
			delta += " "
		}
		_ = writeChunk(map[string]any{
			"id": "mock", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"content": delta},
			}},
		})
		time.Sleep(2 * time.Millisecond)
	}

	for _, tc := range reply.ToolCalls {
		_ = writeChunk(map[string]any{
			"id": "mock", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"tool_calls": []map[string]any{{
					"index": 0, "id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
				}}},
			}},
		})
	}

	finish := "stop"
	if len(reply.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	_ = writeChunk(map[string]any{
		"id": "mock", "object": "chat.completion.chunk", "model": req.Model,
		"choices": []map[string]any{{"index": 0, "finish_reason": finish}},
	})
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	fl.Flush()
}

// DefaultBrain is the scripted demo agent. It is a tiny state machine
// driven by counting the messages in the conversation (the demo never grows
// past one user turn, and tool results arrive as "tool" role messages):
//   - first turn, no tool result yet -> list the project files;
//   - after the listing came back -> run an echo command (this is the step
//     that exercises the approval prompt, the core of the demo);
//   - after that command ran -> wrap up with a final message;
//   - anything else (more turns) -> a polite dead end.
func DefaultBrain(messages []model.Message) model.Reply {
	user := 0
	tools := 0
	for _, m := range messages {
		switch m.Role {
		case "user":
			user++
		case "tool":
			tools++
		}
	}
	switch {
	case user == 1 && tools == 0:
		return model.Reply{
			Content: "Let me look at the project first. ",
			ToolCalls: []model.ToolCall{{
				ID: "mock_list", Type: "function",
				Function: model.FunctionCall{Name: "list_files", Arguments: `{"path": ""}`},
			}},
		}
	case user == 1 && tools == 1:
		return model.Reply{
			ToolCalls: []model.ToolCall{{
				ID: "mock_run", Type: "function",
				Function: model.FunctionCall{Name: "run_command", Arguments: `{"command": "echo Hello from mock agent"}`},
			}},
		}
	case user == 1:
		return model.Reply{
			Content: "Demo finished. The approved command ran inside the sandbox and returned exit code 0. Type /exit to quit.",
		}
	default:
		return model.Reply{
			Content: "Mock agent has no further scripted steps for this turn. Try /new or /exit.",
		}
	}
}
