package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestGenerateImageEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo, err := openRepository(":memory:", "test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	a, err := newApp(ctx, repo, `["alice:secret"]`)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var prompts []string
	started, release := make(chan struct{}, 1), make(chan struct{})
	block := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		prompts = append(prompts, req.Prompt)
		blocking := block
		mu.Unlock()
		if blocking {
			started <- struct{}{}
			<-release
		}
		_, _ = w.Write([]byte(imageReply(base64.StdEncoding.EncodeToString(tinyPNG))))
	}))
	defer upstream.Close()
	server := httptest.NewServer(a.routes())
	defer server.Close()
	defer a.closeStreams()
	alice, _, _ := repo.NewSession(ctx, "host", "", "", "alice")
	do := func(method, path, token, body string, want int) []byte {
		t.Helper()
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, want, result)
		}
		return result
	}

	// Not configured.
	if got := do("GET", "/api/ai/status", alice, "", 200); !bytes.Contains(got, []byte(`false`)) {
		t.Fatalf("status = %s", got)
	}
	do("POST", "/api/ai/images", alice, `{"prompt":"a cat"}`, 503)

	a.ai = &openRouter{key: "k", model: "m", base: upstream.URL, client: upstream.Client()}
	if got := do("GET", "/api/ai/status", alice, "", 200); !bytes.Contains(got, []byte(`true`)) {
		t.Fatalf("status = %s", got)
	}
	do("GET", "/api/ai/status", "", "", 401)
	do("POST", "/api/ai/images", "", `{"prompt":"a cat"}`, 401)
	do("POST", "/api/ai/images", alice, `{"prompt":"   "}`, 400)
	do("POST", "/api/ai/images", alice, `{"prompt":"`+strings.Repeat("é", maxPromptRunes+1)+`"}`, 400)

	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(do("POST", "/api/ai/images", alice, `{"prompt":" a cat "}`, 200), &out); err != nil || out.ID == "" {
		t.Fatalf("no id: %v", err)
	}
	mu.Lock()
	if prompts[len(prompts)-1] != "a cat" {
		t.Fatalf("prompt = %q", prompts)
	}
	block = true
	mu.Unlock()
	served := do("GET", "/api/images/"+out.ID, "", "", 200)
	if !bytes.Equal(served, tinyPNG) {
		t.Fatal("stored image differs")
	}

	// A second request from the same host while one is running is rejected.
	done := make(chan struct{})
	go func() {
		do("POST", "/api/ai/images", alice, `{"prompt":"slow"}`, 200)
		close(done)
	}()
	<-started
	do("POST", "/api/ai/images", alice, `{"prompt":"again"}`, 429)
	close(release)
	<-done
	// The slot is free again.
	mu.Lock()
	block = false
	mu.Unlock()
	do("POST", "/api/ai/images", alice, `{"prompt":"after"}`, 200)
}

func TestGenerateImageUpstreamErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo, err := openRepository(":memory:", "test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	a, err := newApp(ctx, repo, `["alice:secret"]`)
	if err != nil {
		t.Fatal(err)
	}
	status := 500
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"secret upstream detail"}`))
	}))
	defer upstream.Close()
	a.ai = &openRouter{key: "sk-secret", model: "m", base: upstream.URL, client: upstream.Client()}
	server := httptest.NewServer(a.routes())
	defer server.Close()
	defer a.closeStreams()
	alice, _, _ := repo.NewSession(ctx, "host", "", "", "alice")
	for _, c := range []struct{ upstream, want int }{{500, 502}, {401, 502}, {402, 429}, {429, 429}} {
		status = c.upstream
		request, _ := http.NewRequest("POST", server.URL+"/api/ai/images", strings.NewReader(`{"prompt":"x"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+alice)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != c.want {
			t.Fatalf("upstream %d: got %d, want %d", c.upstream, response.StatusCode, c.want)
		}
		if strings.Contains(string(body), "secret") {
			t.Fatalf("leaked upstream detail: %s", body)
		}
	}
}
