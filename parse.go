package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// Usage is token usage normalized across providers. Input excludes cached
// tokens, so input + cache read + cache write = total prompt tokens.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Parsed is what blackbox extracts from a response body.
type Parsed struct {
	Model string
	Usage Usage
	Text  string // the assistant's text output, reassembled if streamed
}

// rawUsage covers the field names both providers use.
type rawUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	PromptTokens             *int64 `json:"prompt_tokens"`
	CompletionTokens         *int64 `json:"completion_tokens"`
	PromptTokensDetails      *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// event is a superset of the shapes seen in Anthropic Messages, OpenAI Chat
// Completions and OpenAI Responses payloads, streamed or not.
type event struct {
	Type    string    `json:"type"`
	Object  string    `json:"object"` // openai: "chat.completion", "response"
	Model   string    `json:"model"`
	Usage   *rawUsage `json:"usage"`
	Message *struct { // anthropic message_start
		Model string    `json:"model"`
		Usage *rawUsage `json:"usage"`
	} `json:"message"`
	Response *struct { // openai responses: response.completed etc.
		Model  string          `json:"model"`
		Usage  *rawUsage       `json:"usage"`
		Output json.RawMessage `json:"output"`
	} `json:"response"`
	Delta   json.RawMessage `json:"delta"` // anthropic {text}, openai responses "string"
	Content []struct {      // anthropic non-streamed
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Choices []struct { // openai chat
		Delta *struct {
			Content string `json:"content"`
		} `json:"delta"`
		Message *struct {
			Content any `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Output json.RawMessage `json:"output"` // openai responses non-streamed
}

// normalize converts provider usage to Usage. OpenAI counts cached tokens
// inside prompt/input tokens; Anthropic reports them separately.
func (r *rawUsage) normalize(provider string) Usage {
	var u Usage
	get := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	u.Output = get(r.OutputTokens) + get(r.CompletionTokens)
	u.CacheWrite = get(r.CacheCreationInputTokens)
	if provider == "anthropic" {
		u.Input = get(r.InputTokens)
		u.CacheRead = get(r.CacheReadInputTokens)
		return u
	}
	total := get(r.PromptTokens) + get(r.InputTokens)
	if r.PromptTokensDetails != nil {
		u.CacheRead += r.PromptTokensDetails.CachedTokens
	}
	if r.InputTokensDetails != nil {
		u.CacheRead += r.InputTokensDetails.CachedTokens
	}
	u.Input = max(total-u.CacheRead, 0)
	return u
}

// merge keeps the largest value seen per field. Streams report usage in
// pieces (input at the start, output at the end) and some repeat running
// totals, so max is correct where sum would double count.
func (u *Usage) merge(o Usage) {
	u.Input = max(u.Input, o.Input)
	u.Output = max(u.Output, o.Output)
	u.CacheRead = max(u.CacheRead, o.CacheRead)
	u.CacheWrite = max(u.CacheWrite, o.CacheWrite)
}

// ParseResponse extracts model, usage and text from a response body.
// It never fails: unknown shapes just yield an empty result.
func ParseResponse(provider string, body []byte, streamed bool) Parsed {
	var p Parsed
	var text strings.Builder
	if !streamed {
		var e event
		if json.Unmarshal(body, &e) == nil {
			p.absorb(provider, &e, &text)
		}
		p.Text = text.String()
		return p
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || data[0] != '{' {
			continue // e.g. "[DONE]"
		}
		var e event
		if json.Unmarshal(data, &e) == nil {
			p.absorb(provider, &e, &text)
		}
	}
	p.Text = text.String()
	return p
}

func (p *Parsed) absorb(provider string, e *event, text *strings.Builder) {
	setModel := func(m string) {
		if m != "" && p.Model == "" {
			p.Model = m
		}
	}
	setModel(e.Model)
	if e.Usage != nil {
		p.Usage.merge(e.Usage.normalize(provider))
	}
	if e.Message != nil {
		setModel(e.Message.Model)
		if e.Message.Usage != nil {
			p.Usage.merge(e.Message.Usage.normalize(provider))
		}
	}
	if e.Response != nil {
		setModel(e.Response.Model)
		if e.Response.Usage != nil {
			p.Usage.merge(e.Response.Usage.normalize(provider))
		}
	}

	switch {
	case e.Type == "content_block_delta": // anthropic stream
		var d struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(e.Delta, &d) == nil {
			text.WriteString(d.Text)
		}
	case e.Type == "response.output_text.delta": // openai responses stream
		var s string
		if json.Unmarshal(e.Delta, &s) == nil {
			text.WriteString(s)
		}
	case e.Type == "message" && len(e.Content) > 0: // anthropic non-stream
		for _, c := range e.Content {
			if c.Type == "text" {
				text.WriteString(c.Text)
			}
		}
	case len(e.Choices) > 0: // openai chat
		c := e.Choices[0]
		if c.Delta != nil {
			text.WriteString(c.Delta.Content)
		} else if c.Message != nil {
			if s, ok := c.Message.Content.(string); ok {
				text.WriteString(s)
			}
		}
	case e.Object == "response" && len(e.Output) > 0: // openai responses non-stream
		text.WriteString(responsesOutputText(e.Output))
	}
}

func responsesOutputText(raw json.RawMessage) string {
	var items []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var b strings.Builder
	for _, it := range items {
		for _, c := range it.Content {
			if c.Type == "output_text" {
				b.WriteString(c.Text)
			}
		}
	}
	return b.String()
}

// requestModel reads "model" from a JSON request body.
func requestModel(body []byte) string {
	var r struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &r)
	return r.Model
}
