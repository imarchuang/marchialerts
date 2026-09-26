package am

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// silenceJSON is the API v2 wire shape for a silence.
type silenceJSON struct {
	ID        string    `json:"id"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
	Status    string    `json:"status,omitempty"`
}

// RegisterHTTP mounts the AM inspection API: silences CRUD + alert status.
func (d *Dispatcher) RegisterHTTP(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v2/silences", d.handlePostSilence)
	mux.HandleFunc("GET /api/v2/silences", d.handleGetSilences)
	mux.HandleFunc("GET /api/v2/silences/{id}", d.handleGetSilence)
	mux.HandleFunc("DELETE /api/v2/silences/{id}", d.handleDeleteSilence)
	mux.HandleFunc("GET /api/v2/alerts", d.handleGetAlerts)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (d *Dispatcher) handlePostSilence(w http.ResponseWriter, r *http.Request) {
	var in silenceJSON
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := d.Silences.Create(Silence{
		ID:        in.ID,
		Matchers:  in.Matchers,
		StartsAt:  in.StartsAt,
		EndsAt:    in.EndsAt,
		CreatedBy: in.CreatedBy,
		Comment:   in.Comment,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"silenceID": id})
}

func (d *Dispatcher) handleGetSilences(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("filter") // pending|active|expired; empty = all
	t := now()
	var out []silenceJSON
	for _, s := range d.Silences.List(state, t) {
		out = append(out, silenceJSON{
			ID: s.ID, Matchers: s.Matchers, StartsAt: s.StartsAt, EndsAt: s.EndsAt,
			CreatedBy: s.CreatedBy, Comment: s.Comment, Status: s.State(t),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Dispatcher) handleGetSilence(w http.ResponseWriter, r *http.Request) {
	s := d.Silences.Get(r.PathValue("id"))
	if s == nil {
		http.Error(w, "silence not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, silenceJSON{
		ID: s.ID, Matchers: s.Matchers, StartsAt: s.StartsAt, EndsAt: s.EndsAt,
		CreatedBy: s.CreatedBy, Comment: s.Comment, Status: s.State(now()),
	})
}

func (d *Dispatcher) handleDeleteSilence(w http.ResponseWriter, r *http.Request) {
	if !d.Silences.Delete(r.PathValue("id"), now()) {
		http.Error(w, "silence not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
}

func (d *Dispatcher) handleGetAlerts(w http.ResponseWriter, r *http.Request) {
	var silenced *bool
	switch strings.ToLower(r.URL.Query().Get("silenced")) {
	case "true":
		v := true
		silenced = &v
	case "false":
		v := false
		silenced = &v
	}
	writeJSON(w, http.StatusOK, d.Alerts(silenced))
}
