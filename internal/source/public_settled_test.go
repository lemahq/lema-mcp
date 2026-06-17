package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSettledDecodesEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/settled" || r.Method != http.MethodPost {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react", "topic": "memo default", "settled": "settled",
			"note":      "surface the prior decision",
			"decisions": []map[string]string{{"ref": "RFC-1", "reason": "do not propose memo default: breaking"}},
		})
	}))
	defer srv.Close()

	res, err := NewPublic(srv.URL, nil).Settled(context.Background(), "react", "memo default")
	if err != nil {
		t.Fatal(err)
	}
	if res.Settled != "settled" || len(res.Decisions) != 1 || res.Decisions[0].Ref != "RFC-1" {
		t.Fatalf("bad decode: %+v", res)
	}
}

func TestSettledNotLoaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := NewPublic(srv.URL, nil).Settled(context.Background(), "nope", "x")
	if err != ErrPublicGraphNotLoaded {
		t.Fatalf("err = %v, want ErrPublicGraphNotLoaded", err)
	}
}
