// db-status: a web page that reports whether this pod can reach each of its
// databases. Every connection goes to 127.0.0.1; the tbot sidecar in the same
// pod turns each local port into an authenticated Teleport database session.
// The application holds no credential and contains no Teleport code.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Target is one database as the app sees it: a localhost port the sidecar
// listens on, plus the user and database name the sidecar was told to use
// (repeated here only so the page can print them next to what the server says).
type Target struct {
	Name     string `yaml:"name"`     // the Teleport database resource name
	Engine   string `yaml:"engine"`   // postgres | mysql | mongodb | redis | clickhouse | cassandra | oracle
	Port     int    `yaml:"port"`     // 127.0.0.1 port the sidecar tunnel listens on
	User     string `yaml:"user"`     // database user the tunnel connects as
	Database string `yaml:"database"` // database / keyspace / service name, engine-specific
}

type Config struct {
	Databases []Target `yaml:"databases"`
}

// Result is one probe outcome, as shown on the page and in /status.json.
type Result struct {
	Name        string  `json:"name"`
	Engine      string  `json:"engine"`
	Port        int     `json:"port"`
	User        string  `json:"user"`
	Database    string  `json:"database,omitempty"`
	OK          bool    `json:"ok"`
	ConnectedAs string  `json:"connected_as,omitempty"` // what the server itself reports
	Version     string  `json:"version,omitempty"`
	LatencyMS   float64 `json:"latency_ms"`
	Error       string  `json:"error,omitempty"`
}

type Report struct {
	Bot       string     `json:"bot"`
	Pod       PodInfo    `json:"pod"`
	Sidecar   Sidecar    `json:"sidecar"`
	Databases []Result   `json:"databases"`
	CheckedAt time.Time  `json:"checked_at"`
	Cached    bool       `json:"cached"`
	Refresh   int        `json:"refresh_seconds"`
}

type PodInfo struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	ServiceAccount string `json:"service_account"`
	Node           string `json:"node"`
}

const (
	probeTimeout = 5 * time.Second
	cacheFor     = 15 * time.Second
	refreshEvery = 30
)

type server struct {
	cfg     Config
	bot     string
	pod     PodInfo
	diag    string
	mu      sync.Mutex
	last    *Report
	lastAt  time.Time
}

func main() {
	path := envOr("DB_STATUS_CONFIG", "/config/databases.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("parse %s: %v", path, err)
	}
	if len(cfg.Databases) == 0 {
		log.Fatalf("%s lists no databases", path)
	}
	s := &server{
		cfg:  cfg,
		bot:  envOr("BOT_NAME", "unknown"),
		diag: envOr("TBOT_DIAG_ADDR", "127.0.0.1:3011"),
		pod: PodInfo{
			Namespace:      os.Getenv("POD_NAMESPACE"),
			Name:           os.Getenv("POD_NAME"),
			ServiceAccount: os.Getenv("POD_SERVICE_ACCOUNT"),
			Node:           os.Getenv("NODE_NAME"),
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/status.json", s.statusJSON)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	addr := envOr("LISTEN", ":8080")
	log.Printf("db-status listening on %s, %d databases, bot %s", addr, len(cfg.Databases), s.bot)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// report probes every database in parallel, or returns the last report if it
// is fresh enough. A demo refreshes the page a lot; the databases should not
// see a session per keystroke.
func (s *server) report(ctx context.Context, force bool) *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && s.last != nil && time.Since(s.lastAt) < cacheFor {
		r := *s.last
		r.Cached = true
		return &r
	}
	results := make([]Result, len(s.cfg.Databases))
	var wg sync.WaitGroup
	for i, t := range s.cfg.Databases {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			results[i] = probe(pctx, t)
		}(i, t)
	}
	sidecar := readSidecar(ctx, s.diag)
	wg.Wait()
	sort.SliceStable(results, func(a, b int) bool { return results[a].Engine < results[b].Engine })
	r := &Report{Bot: s.bot, Pod: s.pod, Sidecar: sidecar, Databases: results, CheckedAt: time.Now().UTC(), Refresh: refreshEvery}
	s.last, s.lastAt = r, time.Now()
	return r
}

func (s *server) statusJSON(w http.ResponseWriter, req *http.Request) {
	r := s.report(req.Context(), req.URL.Query().Has("refresh"))
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
