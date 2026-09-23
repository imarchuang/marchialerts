package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// importSample is the wire shape: {metric, labels, t, v}. t is optional
// unix seconds and defaults to receive time.
type importSample struct {
	Metric string            `json:"metric"`
	Labels map[string]string `json:"labels"`
	T      *int64            `json:"t"`
	V      float64           `json:"v"`
}

// ImportHandler serves POST /api/v1/import with one sample object or an
// array of them (marchimetrics-shaped).
func ImportHandler(st *Store, now func() time.Time) http.HandlerFunc {
	type response struct {
		Imported int `json:"imported"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		body = bytes.TrimSpace(body)
		if len(body) == 0 {
			http.Error(w, "empty body", http.StatusBadRequest)
			return
		}

		var in []importSample
		if body[0] == '[' {
			if err := json.Unmarshal(body, &in); err != nil {
				http.Error(w, "bad JSON array: "+err.Error(), http.StatusBadRequest)
				return
			}
		} else {
			var one importSample
			if err := json.Unmarshal(body, &one); err != nil {
				http.Error(w, "bad JSON object: "+err.Error(), http.StatusBadRequest)
				return
			}
			in = []importSample{one}
		}

		samples := make([]Sample, 0, len(in))
		for i, s := range in {
			if s.Metric == "" {
				http.Error(w, fmt.Sprintf("sample %d: metric is required", i), http.StatusBadRequest)
				return
			}
			t := now()
			if s.T != nil {
				t = time.Unix(*s.T, 0).UTC()
			}
			samples = append(samples, Sample{Metric: s.Metric, Labels: s.Labels, T: t, V: s.V})
		}

		n := st.Add(samples...)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{Imported: n})
	}
}
