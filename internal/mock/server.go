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

type Brain func(messages []model.Message) model.Reply

type Server struct {
	brain Brain
	srv   *http.Server
	ln    net.Listener
}

func New(brain Brain) *Server {
	return &Server{brain: brain}
}

func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

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

func (s *Server) Close() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

func (s *Server) Brain() Brain {
	if s.brain != nil {
		return s.brain
	}
	return DefaultBrain
}

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
