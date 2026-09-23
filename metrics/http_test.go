package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestImportSingleSample(t *testing.T) {
	st := NewStore()
	h := ImportHandler(st, time.Now)

	rec := post(t, h, `{"metric":"cpu_usage","labels":{"job":"api"},"t":1700000000,"v":0.95}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"imported":1`) {
		t.Fatalf("body = %s", rec.Body)
	}

	got := st.Select("cpu_usage", map[string]string{"job": "api"})
	if len(got) != 1 || got[0].V != 0.95 {
		t.Fatalf("store = %v", got)
	}
	if got[0].T != time.Unix(1_700_000_000, 0).UTC() {
		t.Errorf("T = %v, want unix 1700000000", got[0].T)
	}
}

func TestImportArray(t *testing.T) {
	st := NewStore()
	h := ImportHandler(st, time.Now)

	rec := post(t, h, `[
		{"metric":"cpu_usage","labels":{"job":"api","host":"a"},"v":0.9},
		{"metric":"cpu_usage","labels":{"job":"api","host":"b"},"v":0.1}
	]`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"imported":2`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := st.Select("cpu_usage", nil); len(got) != 2 {
		t.Fatalf("store = %d series, want 2", len(got))
	}
}

func TestImportDefaultsTimestampToNow(t *testing.T) {
	st := NewStore()
	fixed := time.Unix(1_700_000_123, 0).UTC()
	h := ImportHandler(st, func() time.Time { return fixed })

	rec := post(t, h, `{"metric":"cpu_usage","v":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := st.Select("cpu_usage", nil)[0].T; !got.Equal(fixed) {
		t.Errorf("T = %v, want %v", got, fixed)
	}
}

func TestImportBadRequests(t *testing.T) {
	st := NewStore()
	h := ImportHandler(st, time.Now)

	for name, body := range map[string]string{
		"empty":          ``,
		"not json":       `{`,
		"missing metric": `{"v":1}`,
	} {
		if rec := post(t, h, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}
