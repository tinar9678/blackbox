package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"time"
)

//go:embed ui
var uiFiles embed.FS

// uiPrefix is where the web UI and its API live. Everything else is proxied.
const uiPrefix = "/_bb"

func (p *Proxy) uiHandler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET "+uiPrefix+"/", http.StripPrefix(uiPrefix, http.FileServerFS(static)))

	mux.HandleFunc("GET "+uiPrefix+"/api/calls", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 200
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		calls, err := p.store.List(limit, r.URL.Query().Get("q"), after)
		writeJSON(w, calls, err)
	})

	mux.HandleFunc("GET "+uiPrefix+"/api/calls/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		c, err := p.store.Get(id)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, c, err)
	})

	mux.HandleFunc("GET "+uiPrefix+"/api/stats", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		day, err := p.store.StatsSince(today)
		if err != nil {
			writeJSON(w, nil, err)
			return
		}
		all, err := p.store.StatsSince(time.Time{})
		writeJSON(w, map[string]Stats{"today": day, "all": all}, err)
	})

	mux.HandleFunc("DELETE "+uiPrefix+"/api/calls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true}, p.store.DeleteAll())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
