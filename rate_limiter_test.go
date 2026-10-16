package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRateLimitsArePerIPAndEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name, path, body string
		burst, allowed   int
	}{
		{"login", "/api/host/login", `{"username":"host","password":"wrong"}`, 10, 401},
		{"join", "/api/join", `{"room":"MISSING","name":"Ada"}`, 60, 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := openRepository(":memory:", "test-signing-key-at-least-32-bytes")
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			app, err := newApp(context.Background(), repo, `["host:password"]`)
			if err != nil {
				t.Fatal(err)
			}
			handler := app.routes()
			for attempt := 0; attempt <= tt.burst; attempt++ {
				request := httptest.NewRequest("POST", tt.path, strings.NewReader(tt.body))
				request.Header.Set("Content-Type", "application/json")
				// Neither changing source ports nor supplying forwarded headers
				// should bypass the limit for the actual connecting IP.
				request.RemoteAddr = fmt.Sprintf("192.0.2.1:%d", 10000+attempt)
				request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", attempt+1))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				want := tt.allowed
				if attempt == tt.burst {
					want = 429
				}
				if response.Code != want {
					t.Fatalf("attempt %d = %d, want %d: %s", attempt+1, response.Code, want, response.Body)
				}
				if want == 429 {
					var body map[string]string
					if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["error"] != "too many attempts; retry in a minute" {
						t.Fatalf("rate limit response = %s, %v", response.Body, err)
					}
					if response.Header().Get("Retry-After") == "" {
						t.Fatal("rate limit response omitted Retry-After")
					}
				}
			}
			request := httptest.NewRequest("POST", tt.path, strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			request.RemoteAddr = "192.0.2.2:10000"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.allowed {
				t.Fatalf("another IP was limited: %d", response.Code)
			}
			otherPath, otherBody, otherStatus := "/api/join", `{"room":"MISSING","name":"Ada"}`, 404
			if tt.name == "join" {
				otherPath, otherBody, otherStatus = "/api/host/login", `{"username":"host","password":"wrong"}`, 401
			}
			request = httptest.NewRequest("POST", otherPath, strings.NewReader(otherBody))
			request.Header.Set("Content-Type", "application/json")
			request.RemoteAddr = "192.0.2.1:10000"
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != otherStatus {
				t.Fatalf("another endpoint shared the exhausted bucket: %d", response.Code)
			}
		})
	}
}
