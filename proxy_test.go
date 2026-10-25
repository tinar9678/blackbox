package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const anthropicJSON = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929",
"content":[{"type":"text","text":"Hello there"}],
"usage":{"input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":5000,"cache_creation_input_tokens":0}}`

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":120,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":300}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Tina"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

const openaiSSE = `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-5.4-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"Hey"}}]}

data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-5.4-mini","choices":[{"index":0,"delta":{"content":" you"}}]}

data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-5.4-mini","choices":[],"usage":{"prompt_tokens":2000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":1500}}}

data: [DONE]

`

// fakeUpstream records what it received and replies with a canned body.
type fakeUpstream struct {
	srv     *httptest.Server
	gotPath string
	gotHdr  http.Header
	gotBody string
}

func newFake(t *testing.T, contentType, body string, status int) *fakeUpstream {
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotPath, f.gotHdr = r.URL.Path, r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		f.gotBody = string(b)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		// Stream in small chunks with flushes, like a real SSE server.
		fl := w.(http.Flusher)
		for _, chunk := range strings.SplitAfter(body, "\n\n") {
			io.WriteString(w, chunk)
			fl.Flush()
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func setup(t *testing.T, anthropic, openai string) (*httptest.Server, *Store) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pricer, _ := LoadPricer(filepath.Join(t.TempDir(), "none.json"))
	au, _ := url.Parse(anthropic)
	ou, _ := url.Parse(openai)
	p := NewProxy(store, pricer, map[string]*url.URL{"anthropic": au, "openai": ou}, 1<<20)
	saved := make(chan struct{}, 10)
	p.onSaved = func(*Call) { saved <- struct{}{} }
	mux := http.NewServeMux()
	mux.Handle(uiPrefix+"/", p.uiHandler())
	mux.Handle("/", p)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store
}

// lastCall waits for the proxy to finish recording, then returns the newest call.
func lastCall(t *testing.T, store *Store) *Call {
	t.Helper()
	for i := 0; i < 100; i++ {
		calls, _ := store.List(1, "", 0)
		if len(calls) == 1 {
			c, err := store.Get(calls[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no call recorded")
	return nil
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAnthropicJSON(t *testing.T) {
	up := newFake(t, "application/json", anthropicJSON, 200)
	srv, store := setup(t, up.srv.URL, "http://unused.invalid")

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "sk-ant-secret-123")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("X-Blackbox-Tag", "exp-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != anthropicJSON {
		t.Errorf("client got a modified body: %s", body)
	}
	if up.gotPath != "/v1/messages" || up.gotHdr.Get("X-Api-Key") != "sk-ant-secret-123" {
		t.Errorf("upstream got path %q key %q", up.gotPath, up.gotHdr.Get("X-Api-Key"))
	}
	if up.gotHdr.Get("X-Blackbox-Tag") != "" {
		t.Error("tag header leaked upstream")
	}

	c := lastCall(t, store)
	if c.Provider != "anthropic" || c.Tag != "exp-1" || c.Status != 200 || c.Streamed {
		t.Errorf("bad call metadata: %+v", c)
	}
	if c.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("model = %q, want the exact version from the response", c.Model)
	}
	if c.InputTokens != 1000 || c.OutputTokens != 200 || c.CacheRead != 5000 {
		t.Errorf("usage = %d/%d/%d", c.InputTokens, c.OutputTokens, c.CacheRead)
	}
	// 1000*3 + 200*15 + 5000*0.30 per million
	if c.CostUSD == nil || !near(*c.CostUSD, (3000+3000+1500)/1e6) {
		t.Errorf("cost = %v", c.CostUSD)
	}
	if c.ResponseText != "Hello there" {
		t.Errorf("text = %q", c.ResponseText)
	}
	if strings.Contains(c.RequestHeaders, "secret") || !strings.Contains(c.RequestHeaders, "[redacted]") {
		t.Errorf("API key not redacted: %s", c.RequestHeaders)
	}
}

func TestAnthropicStream(t *testing.T) {
	up := newFake(t, "text/event-stream", anthropicSSE, 200)
	// Explicit /anthropic prefix routing, with the upstream given a base path.
	srv2, store2 := setup(t, up.srv.URL+"/base", "http://unused.invalid")

	req, _ := http.NewRequest("POST", srv2.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != anthropicSSE {
		t.Errorf("stream was altered")
	}
	if up.gotPath != "/base/v1/messages" {
		t.Errorf("upstream path = %q", up.gotPath)
	}
	c := lastCall(t, store2)
	if !c.Streamed || c.ResponseText != "Hi Tina" || c.TTFTMS == nil {
		t.Errorf("streamed=%v text=%q ttft=%v", c.Streamed, c.ResponseText, c.TTFTMS)
	}
	if c.InputTokens != 120 || c.OutputTokens != 42 || c.CacheWrite != 300 {
		t.Errorf("usage = in %d out %d cw %d", c.InputTokens, c.OutputTokens, c.CacheWrite)
	}
	// 120*4 + 42*20 + 300*5 per million
	if c.CostUSD == nil || !near(*c.CostUSD, (480+840+1500)/1e6) {
		t.Errorf("cost = %v", c.CostUSD)
	}
}

func TestOpenAIStreamWithCache(t *testing.T) {
	up := newFake(t, "text/event-stream; charset=utf-8", openaiSSE, 200)
	srv, store := setup(t, "http://unused.invalid", up.srv.URL)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.4-mini","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-proj-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if up.gotHdr.Get("Authorization") != "Bearer sk-proj-secret" {
		t.Error("auth header not forwarded")
	}
	c := lastCall(t, store)
	if c.Provider != "openai" || c.ResponseText != "Hey you" {
		t.Errorf("provider=%s text=%q", c.Provider, c.ResponseText)
	}
	// OpenAI includes cached tokens in prompt_tokens; we split them out.
	if c.InputTokens != 500 || c.CacheRead != 1500 || c.OutputTokens != 50 {
		t.Errorf("usage = in %d cached %d out %d", c.InputTokens, c.CacheRead, c.OutputTokens)
	}
	if c.CostUSD == nil || !near(*c.CostUSD, (500*0.75+1500*0.075+50*4.5)/1e6) {
		t.Errorf("cost = %v", c.CostUSD)
	}
}

func TestUpstreamErrorStatusIsRecordedUnpriced(t *testing.T) {
	up := newFake(t, "application/json", `{"type":"error","error":{"type":"overloaded_error"}}`, 529)
	srv, store := setup(t, up.srv.URL, "http://unused.invalid")
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, _ := http.DefaultClient.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 529 {
		t.Errorf("client status = %d", resp.StatusCode)
	}
	c := lastCall(t, store)
	if c.Status != 529 || c.CostUSD != nil || c.Model != "claude-opus-5-5" {
		t.Errorf("status=%d cost=%v model=%q", c.Status, c.CostUSD, c.Model)
	}
}

func TestUpstreamDown(t *testing.T) {
	srv, store := setup(t, "http://127.0.0.1:1", "http://unused.invalid")
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if c := lastCall(t, store); c.Status != 502 || c.Error == "" {
		t.Errorf("status=%d err=%q", c.Status, c.Error)
	}
}

func TestUIAPI(t *testing.T) {
	up := newFake(t, "application/json", anthropicJSON, 200)
	srv, store := setup(t, up.srv.URL, "http://unused.invalid")
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("X-Blackbox-Tag", fmt.Sprintf("run-%d", i))
		resp, _ := http.DefaultClient.Do(req)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	lastCall(t, store)
	time.Sleep(50 * time.Millisecond)

	var calls []Call
	getJSONT(t, srv.URL+"/_bb/api/calls?q=run-1", &calls)
	if len(calls) != 1 || calls[0].Tag != "run-1" || calls[0].RequestBody != "" {
		t.Errorf("filtered list = %+v", calls)
	}
	var one Call
	getJSONT(t, fmt.Sprintf("%s/_bb/api/calls/%d", srv.URL, calls[0].ID), &one)
	if one.ResponseText != "Hello there" {
		t.Errorf("detail = %+v", one)
	}
	var stats map[string]Stats
	getJSONT(t, srv.URL+"/_bb/api/stats", &stats)
	if stats["today"].Calls != 3 {
		t.Errorf("stats = %+v", stats)
	}
	resp, _ := http.Get(srv.URL + "/_bb/")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "<title>blackbox</title>") {
		t.Error("UI not served")
	}
}

func getJSONT(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestParseOpenAIResponsesAPI(t *testing.T) {
	body := `{"object":"response","model":"gpt-5.5","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],
	"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":40}}}`
	p := ParseResponse("openai", []byte(body), false)
	if p.Text != "done" || p.Model != "gpt-5.5" || p.Usage != (Usage{Input: 60, Output: 7, CacheRead: 40}) {
		t.Errorf("parsed = %+v", p)
	}
}

func TestPricing(t *testing.T) {
	pr, _ := LoadPricer(filepath.Join(t.TempDir(), "none.json"))
	if pr.Cost("some-local-model", Usage{Input: 10}) != nil {
		t.Error("unknown model should be unpriced")
	}
	long := *pr.Cost("claude-haiku-5-5", Usage{Input: 1_000_000})
	if !near(*pr.Cost("claude-haiku-5-5", Usage{Input: 50_000}), 50_000*0.10/1e6) {
		t.Error("short-prompt tier wrong")
	}
	if !near(long, 0.50) {
		t.Errorf("long-prompt tier = %v", long)
	}
	// Longest prefix wins: gpt-5.4-mini must not match gpt-5.4.
	if !near(*pr.Cost("gpt-5.4-mini-2026-03-01", Usage{Output: 1_000_000}), 4.50) {
		t.Error("prefix match picked the wrong model")
	}
}
