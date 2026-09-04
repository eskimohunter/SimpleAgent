// Package model defines the wire types shared with the OpenAI-compatible
// chat API and the Client that streams from it.
//
// There are two layers here: the structs that get JSON-encoded and sent to
// the model server (Message, ToolCall, ChatRequest, Tool), and the Go-native
// types the rest of the program works with (Reply, plus the constructors for
// Message). Keeping them separate means the JSON shapes stay exactly what the
// API expects while callers get convenient builders.
package model

import (
	"encoding/json"
	"fmt"
)

// Message is one entry of the conversation history sent to the model.
//
// Content is a *string (pointer to a string), not a string: a nil pointer
// marshals to JSON as nothing (the field is omitted), which the API requires
// for assistant messages that only contain tool calls. TextMessage,
// ToolResultMessage and AssistantMessage build valid Message values so
// callers rarely touch this struct's fields directly.
type Message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is the model asking the harness to invoke a tool. The arguments
// are opaque JSON (a string), decoded per tool by the agent package.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the tool to run and carries its JSON arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// TextMessage builds a plain user or system message. Note the local copy:
// Go escapes the parameter into a heap variable we can point at, because
// taking the address of a function parameter directly is not allowed.
func TextMessage(role, content string) Message {
	c := content
	return Message{Role: role, Content: &c}
}

// ToolResultMessage reports a tool call's outcome back to the model. The
// protocol requires role "tool" plus the ToolCallID of the call it answers.
func ToolResultMessage(toolCallID, content string) Message {
	c := content
	return Message{Role: "tool", Content: &c, ToolCallID: toolCallID}
}

// AssistantMessage records the model's reply. Content stays nil when the
// reply only carries tool calls (the API forbids an empty content string
// together with tool_calls in some servers).
func AssistantMessage(content string, calls []ToolCall) Message {
	var c *string
	if content != "" {
		cc := content
		c = &cc
	}
	return Message{Role: "assistant", Content: c, ToolCalls: calls}
}

// FunctionSchema is the JSON Schema description of one tool that gets sent
// to the model so it knows what it may call and with which arguments.
type FunctionSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Tool wraps a FunctionSchema in the "function" envelope the API expects.
type Tool struct {
	Type     string         `json:"type"`
	Function FunctionSchema `json:"function"`
}

// NewTool assembles a Tool from its parts. properties maps each argument
// name to a JSON Schema fragment ("type", "description", ...) and required
// lists which arguments the model must always supply. additionalProperties
// is locked to false so the model cannot smuggle in unknown arguments.
func NewTool(name, description string, properties map[string]any, required []string) Tool {
	return Tool{
		Type: "function",
		Function: FunctionSchema{
			Name:        name,
			Description: description,
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           properties,
				"required":             required,
				"additionalProperties": false,
			},
		},
	}
}

// ChatRequest is the JSON body sent to POST /chat/completions.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream"`
}

// chatChunk mirrors the shape of one SSE data event ("chunk") the server
// streams back. Every field is optional because chunks are incremental:
// content arrives as partial words, tool calls arrive one fragment at a time.
// The unused ones (id, object, model, ...) are deliberately not modeled.
type chatChunk struct {
	Error   *struct{ Message string } `json:"error"`
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// Reply is the assembled result of one Chat call: the full assistant text,
// any tool calls it made (in order), and why it stopped ("stop",
// "tool_calls", ...).
type Reply struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
}

// JSON serializes the reply (used by the mock server and tests).
func (r Reply) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// String gives a human-readable one-line summary of the reply.
func (r Reply) String() string {
	if r.Content != "" {
		return r.Content
	}
	if len(r.ToolCalls) > 0 {
		return fmt.Sprintf("%d tool call(s)", len(r.ToolCalls))
	}
	return ""
}
