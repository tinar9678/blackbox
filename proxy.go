package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const tagHeader = "X-Blackbox-Tag"

// Headers that carry credentials are never written to the database.
var secretHeaders = map[string]bool{
	"Authorization":       true,
	"X-Api-Key":           true,
	"Api-Key":             true,
	"Cookie":              true,
	"Set-Cookie":          true,
	"Proxy-Authorization": true,
}

type Proxy struct {
	store     *Store
	pricer    *Pricer
	upstreams map[string]*url.URL // "anthropic", "openai"
	maxBody   int                 // bytes of each body to keep in the database
	rp        *httputil.ReverseProxy
	onSaved   func(*Call) // optional hook, used for the console log line
}

type ctxKey struct{}

// recording tracks one in-flight call from request to final response byte.
type recording struct {
	call      Call
	upstream  *url.URL
	path      string // path to request on the upstream
	reqBody   []byte
	start     time.Time
	firstByte time.Time
	resp      bytes.Buffer
	truncated bool
	once      sync.Once
}

func NewProxy(store *Store, pricer *Pricer, upstreams map[string]*url.URL, maxBody int) *Proxy {
	p := &Proxy{store: store, pricer: pricer, upstreams: upstreams, maxBody: maxBody}
	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
		FlushInterval:  -1, // flush every write so streamed tokens arrive live
	}
	return p
}

// route picks the provider. An explicit /anthropic or /openai prefix wins;
// otherwise Anthropic SDKs are recognized by their headers.
func route(r *http.Request) (provider, path string) {
	for _, name := range []string{"anthropic", "openai"} {
		prefix := "/" + name
		if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			return name, strings.TrimPrefix(r.URL.Path, prefix)
		}
	}
	if r.Header.Get("Anthropic-Version") != "" || r.Header.Get("X-Api-Key") != "" {
		return "anthropic", r.URL.Path
	}
	return "openai", r.URL.Path
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	provider, path := route(r)
	upstream, ok := p.upstreams[provider]
	if !ok {
		http.Error(w, "blackbox: no upstream for "+provider, http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "blackbox: reading request: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	tag := r.Header.Get(tagHeader)
	r.Header.Del(tagHeader) // blackbox-only; never sent upstream

	rec := &recording{
		upstream: upstream,
		path:     path,
		reqBody:  body,
		start:    time.Now(),
		call: Call{
			StartedAt:      time.Now().UTC().Format(timeFormat),
			Provider:       provider,
			Method:         r.Method,
			Path:           path,
			Model:          requestModel(body),
			Tag:            tag,
			RequestHeaders: headersJSON(r.Header),
			RequestBody:    p.clip(body),
		},
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, rec)))
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	rec := pr.In.Context().Value(ctxKey{}).(*recording)
	out := pr.Out.URL
	out.Scheme = rec.upstream.Scheme
	out.Host = rec.upstream.Host
	out.Path = strings.TrimSuffix(rec.upstream.Path, "/") + rec.path
	out.RawPath = ""
	pr.Out.Host = ""
	// Let Go's transport negotiate gzip itself; it then hands us a
	// decompressed body we can record, and clients still get plain bytes.
	pr.Out.Header.Del("Accept-Encoding")
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	rec := resp.Request.Context().Value(ctxKey{}).(*recording)
	rec.call.Status = resp.StatusCode
	rec.call.Streamed = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	rec.call.ResponseHeaders = headersJSON(resp.Header)
	resp.Body = &teeBody{rc: resp.Body, rec: rec, p: p}
	return nil
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	rec := r.Context().Value(ctxKey{}).(*recording)
	status := http.StatusBadGateway
	if errors.Is(err, context.Canceled) {
		status = 499 // client went away before the upstream answered
	}
	rec.call.Status = status
	p.finish(rec, err.Error())
	if status == http.StatusBadGateway {
		http.Error(w, "blackbox: upstream error: "+err.Error(), status)
	}
}

// teeBody copies the response into the recording as it streams to the client.
type teeBody struct {
	rc  io.ReadCloser
	rec *recording
	p   *Proxy
	eof bool
}

func (t *teeBody) Read(b []byte) (int, error) {
	n, err := t.rc.Read(b)
	if n > 0 {
		if t.rec.firstByte.IsZero() {
			t.rec.firstByte = time.Now()
		}
		room := max(t.p.maxBody-t.rec.resp.Len(), 0)
		t.rec.resp.Write(b[:min(n, room)])
		if n > room {
			t.rec.truncated = true
		}
	}
	if err == io.EOF {
		t.eof = true
		t.p.finish(t.rec, "")
	}
	return n, err
}

func (t *teeBody) Close() error {
	if !t.eof {
		t.p.finish(t.rec, "response not fully read (client disconnected?)")
	}
	return t.rc.Close()
}

func (p *Proxy) finish(rec *recording, errMsg string) {
	rec.once.Do(func() {
		c := &rec.call
		c.DurationMS = time.Since(rec.start).Milliseconds()
		if !rec.firstByte.IsZero() {
			ms := rec.firstByte.Sub(rec.start).Milliseconds()
			c.TTFTMS = &ms
		}
		c.Error = errMsg
		body := rec.resp.Bytes()
		c.ResponseBody = string(body)
		if rec.truncated {
			c.ResponseBody += fmt.Sprintf("\n…[blackbox: truncated at %d bytes]", p.maxBody)
		}
		parsed := ParseResponse(c.Provider, body, c.Streamed)
		if parsed.Model != "" {
			c.Model = parsed.Model // the response names the exact model version
		}
		c.ResponseText = parsed.Text
		c.InputTokens = parsed.Usage.Input
		c.OutputTokens = parsed.Usage.Output
		c.CacheRead = parsed.Usage.CacheRead
		c.CacheWrite = parsed.Usage.CacheWrite
		if c.Status < 400 && c.Model != "" {
			c.CostUSD = p.pricer.Cost(c.Model, parsed.Usage)
		}
		id, err := p.store.Insert(c)
		if err != nil {
			log.Printf("blackbox: saving call: %v", err)
			return
		}
		c.ID = id
		if p.onSaved != nil {
			p.onSaved(c)
		}
	})
}

func (p *Proxy) clip(b []byte) string {
	if len(b) <= p.maxBody {
		return string(b)
	}
	return string(b[:p.maxBody]) + fmt.Sprintf("\n…[blackbox: truncated at %d bytes]", p.maxBody)
}

func headersJSON(h http.Header) string {
	out := map[string]string{}
	for k, v := range h {
		k = http.CanonicalHeaderKey(k)
		if secretHeaders[k] {
			out[k] = "[redacted]"
			continue
		}
		out[k] = strings.Join(v, ", ")
	}
	b, _ := json.Marshal(out)
	return string(b)
}
