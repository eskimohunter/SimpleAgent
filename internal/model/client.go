package model

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Client struct {
	BaseURL  string
	APIKey   string
	Insecure bool
	HTTP     *http.Client
}

func NewClient(baseURL, apiKey string, insecure bool, timeout time.Duration) *Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
	}
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		APIKey:   apiKey,
		Insecure: insecure,
		HTTP: &http.Client{
			Transport: tr,
			Timeout:   timeout,
		},
	}
}

type ChatOptions struct {
	Stream func(text string)
}

func (c *Client) Chat(ctx context.Context, req ChatRequest, onText func(string)) (Reply, error) {
	if req.ToolChoice == "" && len(req.Tools) > 0 {
		req.ToolChoice = "auto"
	}
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return Reply{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Reply{}, fmt.Errorf("request to model %s failed: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return Reply{}, fmt.Errorf("model server returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	var reply Reply
	toolByIndex := map[int]ToolCall{}
	var toolOrder []int
	seenFinish := false

	onLine := func(line string) error {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			return nil
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			seenFinish = true
			return nil
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil
		}
		if chunk.Error != nil {
			return fmt.Errorf("model error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			return nil
		}
		ch := chunk.Choices[0]
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			reply.Content += *ch.Delta.Content
			if onText != nil {
				onText(*ch.Delta.Content)
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			} else if len(toolOrder) > 0 {
				idx = toolOrder[len(toolOrder)-1]
			}
			cur, ok := toolByIndex[idx]
			if !ok {
				toolOrder = append(toolOrder, idx)
				cur = ToolCall{Type: "function"}
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Type != "" {
				cur.Type = tc.Type
			}
			if tc.Function != nil {
				if tc.Function.Name != "" {
					cur.Function.Name = tc.Function.Name
				}
				cur.Function.Arguments += tc.Function.Arguments
			}
			toolByIndex[idx] = cur
		}
		if ch.FinishReason != nil {
			reply.FinishReason = *ch.FinishReason
		}
		return nil
	}

	sc := bufio.NewScanner(bufio.NewReader(resp.Body))
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if err := onLine(sc.Text()); err != nil {
			return Reply{}, err
		}
		if seenFinish {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return Reply{}, fmt.Errorf("stream read: %w", err)
	}
	if reply.Content == "" && len(toolByIndex) == 0 && reply.FinishReason == "" {
		return Reply{}, fmt.Errorf("model returned an empty response (no content, no tool calls)")
	}

	sort.Ints(toolOrder)
	for _, idx := range toolOrder {
		tc := toolByIndex[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", idx)
		}
		reply.ToolCalls = append(reply.ToolCalls, tc)
	}
	return reply, nil
}
