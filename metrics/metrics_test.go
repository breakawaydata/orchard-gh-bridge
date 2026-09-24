package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteText(t *testing.T) {
	r := NewRegistry()
	g := r.NewGaugeVec("test_excluded", "Excluded workers.", "worker", "reason")
	c := r.NewCounterVec("test_pruned_total", "Pruned workers.")

	g.Set(1, "b", "paused")
	g.Set(1, "a", `dup "label"`)
	c.Inc()
	c.Inc()

	var sb strings.Builder
	if err := r.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	want := `# HELP test_excluded Excluded workers.
# TYPE test_excluded gauge
test_excluded{worker="a",reason="dup \"label\""} 1
test_excluded{worker="b",reason="paused"} 1
# HELP test_pruned_total Pruned workers.
# TYPE test_pruned_total counter
test_pruned_total 2
`
	if got := sb.String(); got != want {
		t.Errorf("WriteText =\n%s\nwant\n%s", got, want)
	}
}

func TestGaugeDelete(t *testing.T) {
	r := NewRegistry()
	g := r.NewGaugeVec("g", "h", "k")
	g.Set(1, "x")
	if _, ok := g.Value("x"); !ok {
		t.Fatal("series missing after Set")
	}
	g.Delete("x")
	if _, ok := g.Value("x"); ok {
		t.Error("series still present after Delete")
	}
}

func TestNilSafe(t *testing.T) {
	var r *Registry
	g := r.NewGaugeVec("g", "h", "k")
	c := r.NewCounterVec("c", "h")
	g.Set(1, "x")
	g.Delete("x")
	c.Inc()
	if c.Value() != 0 {
		t.Error("nil counter reported a value")
	}
}

func TestHandler(t *testing.T) {
	r := NewRegistry()
	r.NewGaugeVec("up_gauge", "h").Set(1)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "up_gauge 1\n") {
		t.Errorf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
}
