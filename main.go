// blackbox is a local flight recorder for LLM API calls.
//
// Point your SDK's base URL at it; every request and response is passed
// through untouched and logged to SQLite, viewable at /_bb/.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var version = "0.1.0"

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:7878", "address to listen on")
	dataDir := flag.String("data", filepath.Join(home, ".blackbox"), "directory for blackbox.db and prices.json")
	anthropicURL := flag.String("anthropic-upstream", "https://api.anthropic.com", "Anthropic API base URL")
	openaiURL := flag.String("openai-upstream", "https://api.openai.com", "OpenAI API base URL")
	maxBodyMB := flag.Int("max-body-mb", 10, "max MB of each request/response body to store (traffic is never cut)")
	quiet := flag.Bool("quiet", false, "don't print a line per call")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("blackbox", version)
		return
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(*dataDir, "blackbox.db"))
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer store.Close()

	pricer, err := LoadPricer(filepath.Join(*dataDir, "prices.json"))
	if err != nil {
		log.Fatalf("loading prices: %v", err)
	}

	upstreams := map[string]*url.URL{}
	for name, raw := range map[string]string{"anthropic": *anthropicURL, "openai": *openaiURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			log.Fatalf("bad %s upstream %q", name, raw)
		}
		upstreams[name] = u
	}

	proxy := NewProxy(store, pricer, upstreams, *maxBodyMB<<20)
	if !*quiet {
		proxy.onSaved = logCall
	}

	mux := http.NewServeMux()
	ui := proxy.uiHandler()
	mux.Handle(uiPrefix+"/", ui)
	mux.Handle(uiPrefix, http.RedirectHandler(uiPrefix+"/", http.StatusFound))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// A browser hitting the root wants the UI, not a proxied request.
		if r.URL.Path == "/" && r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, uiPrefix+"/", http.StatusFound)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	base := "http://" + *addr
	fmt.Printf(`
  ▪ blackbox %s  recording to %s

    UI         %s%s/
    Anthropic  export ANTHROPIC_BASE_URL=%s
    OpenAI     export OPENAI_BASE_URL=%s/v1

`, version, filepath.Join(*dataDir, "blackbox.db"), base, uiPrefix, base, base)

	log.Fatal(http.ListenAndServe(*addr, mux))
}

func logCall(c *Call) {
	cost := "    —   "
	if c.CostUSD != nil {
		cost = fmt.Sprintf("$%.4f", *c.CostUSD)
	}
	model := c.Model
	if model == "" {
		model = c.Path
	}
	extra := ""
	if c.Tag != "" {
		extra = " [" + c.Tag + "]"
	}
	if c.Error != "" {
		extra += " ! " + c.Error
	}
	fmt.Printf("  #%-5d %d  %-9s %-28s %7s → %-6s %s  %5.1fs%s\n",
		c.ID, c.Status, c.Provider, model,
		humanTokens(c.InputTokens+c.CacheRead+c.CacheWrite), humanTokens(c.OutputTokens),
		cost, float64(c.DurationMS)/1000, extra)
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}
