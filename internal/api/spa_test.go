package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSPAHandler(t *testing.T) {
	root := t.TempDir()
	web := filepath.Join(root, "web")
	if err := os.MkdirAll(filepath.Join(web, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>shell</html>"), 0o644)
	_ = os.WriteFile(filepath.Join(web, "assets", "app.js"), []byte("console.log(1)"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "secret.txt"), []byte("top secret"), 0o644)
	h := spaHandler(web)

	get := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = p // bypass client-side normalization
		h(rec, req)
		return rec
	}
	if rec := get("/assets/app.js"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "console.log") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	if rec := get("/jobs/job_123"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "shell") {
		t.Fatalf("client route should get the SPA shell: %d %q", rec.Code, rec.Body.String())
	} else if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Fatalf("SPA shell must carry a strict CSP, got %q", csp)
	}
	for _, p := range []string{"/../secret.txt", "/..\\secret.txt", "/assets/..\\..\\secret.txt"} {
		if rec := get(p); strings.Contains(rec.Body.String(), "top secret") {
			t.Fatalf("%q escaped the web root", p)
		}
	}
	if rec := get("/api/v1/nope"); rec.Code != 404 {
		t.Fatalf("API paths must not fall through to the SPA: %d", rec.Code)
	}
}
