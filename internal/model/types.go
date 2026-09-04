package model

import (
	"encoding/json"
	"fmt"
)

type Message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func TextMessage(role, content string) Message {
	c := content
	return Message{Role: role, Content: &c}
}

func ToolResultMessage(toolCallID, content string) Message {
	c := content
	return Message{Role: "tool", Content: &c, ToolCallID: toolCallID}
}

func AssistantMessage(content string, calls []ToolCall) Message {
	var c *string
	if content != "" {
		cc := content
		c = &cc
	}
	return Message{Role: "assistant", Content: c, ToolCalls: calls}
}

type FunctionSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type Tool struct {
	Type     string         `json:"type"`
	Function FunctionSchema `json:"function"`
}

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

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream"`
}

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

type Reply struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
}

func (r Reply) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

func (r Reply) String() string {
	if r.Content != "" {
		return r.Content
	}
	if len(r.ToolCalls) > 0 {
		return fmt.Sprintf("%d tool call(s)", len(r.ToolCalls))
	}
	return ""
}
