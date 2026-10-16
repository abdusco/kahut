package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseAuthUsers(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		wantError   bool
	}{
		{"two users", `["alice:secret","bob:other"]`, false},
		{"password contains colons", `["alice:secret:with:colons"]`, false},
		{"case sensitive names", `["alice:secret","Alice:other"]`, false},
		{"missing", "", true},
		{"empty", `[]`, true},
		{"null", `null`, true},
		{"object", `{"alice":"secret"}`, true},
		{"non-string entry", `[1]`, true},
		{"no separator", `["alice"]`, true},
		{"no username", `[":secret"]`, true},
		{"no password", `["alice:"]`, true},
		{"blank password", `["alice:  "]`, true},
		{"username whitespace", `[" alice:secret"]`, true},
		{"username control character", `["alice\n:secret"]`, true},
		{"duplicate username", `["alice:secret","alice:other"]`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			users, err := parseAuthUsers(tt.input)
			if (err != nil) != tt.wantError {
				t.Fatalf("parseAuthUsers error = %v, want error %v", err, tt.wantError)
			}
			if err != nil {
				if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "other") {
					t.Fatal("configuration error exposed a password")
				}
				return
			}
			if tt.name == "password contains colons" && users["alice"] != sha256.Sum256([]byte("secret:with:colons")) {
				t.Fatal("password was split at more than the first colon")
			}
		})
	}
}

func TestAccountLogin(t *testing.T) {
	ctx := context.Background()
	s, err := openRepository(":memory:", "test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := newApp(ctx, s, `["alice:secret","bob:other:password"]`)
	if err != nil {
		t.Fatal(err)
	}
	handler := a.routes()
	for _, tt := range []struct {
		name, username, password string
		want                     int
	}{
		{"first user", "alice", "secret", 200},
		{"second user", "bob", "other:password", 200},
		{"wrong password", "alice", "wrong", 401},
		{"unknown user", "charlie", "secret", 401},
		{"case sensitive username", "Alice", "secret", 401},
		{"missing username", "", "secret", 401},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"username": tt.username, "password": tt.password})
			request := httptest.NewRequest("POST", "/api/host/login", strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.want {
				t.Fatalf("login = %d, want %d: %s", response.Code, tt.want, response.Body)
			}
			if tt.want == 401 {
				if !strings.Contains(response.Body.String(), "incorrect username or password") {
					t.Fatal("failed logins must use the same message")
				}
				return
			}
			var login struct {
				Token   string  `json:"token"`
				Session Session `json:"session"`
			}
			if err = json.Unmarshal(response.Body.Bytes(), &login); err != nil {
				t.Fatal(err)
			}
			stored, err := s.Session(ctx, login.Token)
			if err != nil || stored.Username != tt.username || login.Session.Username != tt.username {
				t.Fatalf("session does not identify account: %+v, %v", stored, err)
			}
		})
	}
}
