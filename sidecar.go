package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// Sidecar is tbot's own view of itself, read from its diagnostics endpoint
// (/readyz on --diag-addr). It is the only thing on the page that comes from
// Teleport rather than from a database.
type Sidecar struct {
	Reachable bool             `json:"reachable"`
	Status    string           `json:"status,omitempty"`
	Services  []SidecarService `json:"services,omitempty"`
	Error     string           `json:"error,omitempty"`
}

type SidecarService struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func readSidecar(ctx context.Context, addr string) Sidecar {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		return Sidecar{Error: err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Sidecar{Error: err.Error()}
	}
	defer resp.Body.Close()
	var body struct {
		Status   string `json:"status"`
		Services map[string]struct {
			Status    string `json:"status"`
			Reason    string `json:"reason"`
			UpdatedAt string `json:"updated_at"`
		} `json:"services"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Sidecar{Reachable: true, Status: resp.Status, Error: "unexpected /readyz body: " + err.Error()}
	}
	out := Sidecar{Reachable: true, Status: body.Status}
	for name, svc := range body.Services {
		out.Services = append(out.Services, SidecarService{Name: name, Status: svc.Status, Reason: svc.Reason, UpdatedAt: svc.UpdatedAt})
	}
	sort.Slice(out.Services, func(a, b int) bool { return out.Services[a].Name < out.Services[b].Name })
	return out
}
