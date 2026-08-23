package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestMutationIdempotencyReplaysSuccess(t *testing.T) {
	mutations = newMutationCache(filepath.Join(t.TempDir(), "mutations.json"))
	calls := 0
	h := idempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, 200, map[string]int{"calls": calls})
	}))
	for n := 0; n < 2; n++ {
		req := httptest.NewRequest(http.MethodPost, "/edit/id", nil)
		req.Header.Set("Idempotency-Key", "same-operation")
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != 200 || res.Body.String() != "{\"calls\":1}\n" {
			t.Fatalf("unexpected replay: %d %q", res.Code, res.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times", calls)
	}
}
