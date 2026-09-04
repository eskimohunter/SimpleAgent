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

// Client talks to an OpenAI-compatible model server over HTTP and parses its
// server-sent-event (SSE) stream. It is the ONLY code in the harness that
// touches the network: a single POST /chat/completions per model request.
type Client struct {
	BaseURL  string // endpoint base, e.g. http://192.168.1.50:8000/v1
	APIKey   string // sent as "Authorization: Bearer <key>" when non-empty
	Insecure bool   // skip TLS certificate verification (self-signed LAN servers)
	HTTP     *http.Client
}

// NewClient builds a client with a dedicated transport so the TLS setting
// applies to every request, and a total per-request timeout.
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

// ChatOptions is reserved for future request options; today the engine only
// needs the streaming callback form of Chat.
type ChatOptions struct {
	Stream func(text string)
}

// Chat performs one streaming chat completion and returns the fully
// reassembled reply. onText, when non-nil, is invoked with each content
// fragment as it arrives, letting the UI print the answer word by word while
// it is still being generated.
//
// Streaming reassembly is the interesting part: the server does not send the
// answer in one piece but as many small "chunks" (see chatChunk in
// types.go). Text arrives as fragments that we concatenate; a tool call
// arrives as a series of fragments that must be joined by index (a model can
// request several tools in one reply, and their pieces are interleaved).
func (c *Client) Chat(ctx context.Context, req ChatRequest, onText func(string)) (Reply, error) {
	if req.ToolChoice == "" && len(req.Tools) > 0 {
		req.ToolChoice = "auto"
	}
	// The harness always streams; there is no non-streaming mode.
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

	// http.NewRequestWithContext already bound ctx into the request, so
	// cancelling ctx (Ctrl+C, timeouts) aborts this HTTP call mid-flight.
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Reply{}, fmt.Errorf("request to model %s failed: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Read a bounded amount of the error body to include the server's
		// own message (e.g. "invalid api key") in the returned error.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return Reply{}, fmt.Errorf("model server returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	var reply Reply
	toolByIndex := map[int]ToolCall{} // partial tool calls, keyed by the chunk's index field
	var toolOrder []int               // order in which tool-call indexes first appeared
	seenFinish := false

	// onLine handles a single line of the SSE stream. SSE frames look like
	// "data: <json>\n\n" (blank line separates frames); heartbeats and
	// comment lines are ignored. Errors that matter are returned as Go
	// errors; malformed JSON chunks are skipped - the next chunk may be fine.
	onLine := func(line string) error {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			return nil
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			// The server's end-of-stream marker.
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
			// Text fragment: append to the accumulated reply and forward it
			// to the UI callback for live streaming display.
			reply.Content += *ch.Delta.Content
			if onText != nil {
				onText(*ch.Delta.Content)
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			// Reassemble one tool call from its fragments. Each fragment
			// names an index; all fragments with the same index belong to
			// the same tool call. If the server omits the index we assume
			// the call that was started last.
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
				// The arguments JSON arrives in slices that must simply be
				// concatenated to reconstruct the full JSON document.
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

	// Emit the reassembled tool calls in the order they first appeared and
	// synthesize an ID for any call the server forgot to identify. The model
	// must echo these IDs back when we return tool results, so every call
	// needs one.
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
