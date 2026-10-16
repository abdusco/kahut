package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeOpenRouter(t *testing.T, status int, body string) (*openRouter, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(r.Context())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &openRouter{key: "k", model: "m/img", base: srv.URL, client: srv.Client()}, &got
}

func imageReply(b64 string) string {
	b, _ := json.Marshal(map[string]any{"data": []any{map[string]string{"b64_json": b64}}})
	return string(b)
}

func TestOpenRouterGenerate(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("pixels"))
	o, req := fakeOpenRouter(t, 200, imageReply(png))
	data, err := o.Generate(context.Background(), "a cat")
	if err != nil || string(data) != "pixels" {
		t.Fatalf("got %q, %v", data, err)
	}
	if req.Header.Get("Authorization") != "Bearer k" || req.URL.Path != "/images" {
		t.Fatalf("bad request: %s %v", req.URL.Path, req.Header)
	}
}

func TestOpenRouterGenerateFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unauthorized", 401, `{}`, errAIUpstream},
		{"out of credit", 402, `{}`, errAIRateLimit},
		{"rate limited", 429, `{}`, errAIRateLimit},
		{"bad json", 200, `nope`, errAIUpstream},
		{"no data", 200, `{"data":[]}`, errAINoImage},
		{"empty image", 200, imageReply(""), errAINoImage},
		{"bad base64", 200, imageReply("!!!"), errAINoImage},
		{"too large", 200, imageReply(strings.Repeat("AAAA", maxImageBytes/3+1000)), errAIImageLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, _ := fakeOpenRouter(t, c.status, c.body)
			if _, err := o.Generate(context.Background(), "x"); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestNewOpenRouterNeedsKeyAndModel(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "k")
	t.Setenv("OPENROUTER_MODEL", "")
	if newOpenRouter() != nil {
		t.Fatal("expected nil without a model")
	}
	t.Setenv("OPENROUTER_MODEL", "m")
	if o := newOpenRouter(); o == nil || o.base != defaultOpenRouterBase {
		t.Fatalf("got %+v", o)
	}
}
