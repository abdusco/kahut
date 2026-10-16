package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestAPIFlowAndPrivateLiveState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	a, err := newApp(ctx, s, "test-password")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.routes())
	defer server.Close()
	defer a.closeStreams()
	call := func(method, path, token string, body any, want int) map[string]any {
		t.Helper()
		data, _ := json.Marshal(body)
		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var result map[string]any
		if err = json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %+v", method, path, res.StatusCode, want, result)
		}
		return result
	}
	call("POST", "/api/host/login", "", map[string]string{"password": "wrong"}, 401)
	login := call("POST", "/api/host/login", "", map[string]string{"password": "test-password"}, 200)
	host := login["token"].(string)
	quiz := call("POST", "/api/quizzes", host, testQuiz(), 200)
	id := quiz["id"].(string)
	exported := call("GET", "/api/quizzes/"+id+"/export", host, nil, 200)
	if _, ok := exported["id"]; ok {
		t.Fatal("export included internal identifier")
	}
	call("POST", "/api/quizzes/import", host, exported, 200)
	room := call("POST", "/api/rooms", host, map[string]string{"quizId": id}, 200)["room"].(string)
	if len(room) != 8 || strings.Trim(room, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
		t.Fatalf("invalid room code %q", room)
	}
	join := call("POST", "/api/join", "", map[string]string{"room": strings.ToLower(room), "name": "Ada"}, 200)
	player := join["token"].(string)
	call("POST", "/api/join", "", map[string]string{"room": room, "name": "ADA"}, 409)
	call("GET", "/api/quizzes/"+id, player, nil, 403)
	call("POST", "/api/action", player, map[string]any{"room": room, "action": "start", "revision": 1}, 403)
	call("POST", "/api/action", player, map[string]any{"room": "WRONG", "action": "answer", "revision": 1, "question": 0, "choice": 0}, 403)
	call("POST", "/api/action", host, map[string]any{"room": room, "action": "start", "revision": 1}, 200)
	second := call("POST", "/api/join", "", map[string]string{"room": room, "name": "Lin"}, 200)
	call("POST", "/api/action", second["token"].(string), map[string]any{"room": room, "action": "answer", "revision": 2, "question": 0, "choice": 0}, 409)
	tunnelHeaders := http.Header{"Origin": {"https://kahut.ngrok-free.app"}}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/live?token="+player, &websocket.DialOptions{HTTPHeader: tunnelHeaders})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	unauthenticated, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/live?token=invalid", &websocket.DialOptions{HTTPHeader: tunnelHeaders})
	if unauthenticated != nil {
		unauthenticated.CloseNow()
		t.Fatal("cross-origin WebSocket accepted an invalid session")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid cross-origin WebSocket session: response %v, error %v", response, err)
	}
	readCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	_, raw, err := conn.Read(readCtx)
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		Kind     string         `json:"kind"`
		Snapshot map[string]any `json:"snapshot"`
	}
	if err = json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	if first.Kind != "state" || first.Snapshot["phase"] != "question" {
		t.Fatalf("initial state missing: %s", raw)
	}
	if strings.Contains(string(raw), "correctIndex") || strings.Contains(string(raw), "Gold?") {
		t.Fatalf("private state leaked: %s", raw)
	}
	call("POST", "/api/action", player, map[string]any{"room": room, "action": "answer", "revision": 2, "question": 0, "choice": 0}, 200)
	call("POST", "/api/action", player, map[string]any{"room": room, "action": "answer", "revision": 2, "question": 0, "choice": 0}, 409)
	_, raw, err = conn.Read(readCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"phase":"results"`) || !strings.Contains(string(raw), "correctIndex") {
		t.Fatalf("results were not broadcast: %s", raw)
	}
	updated := testQuiz()
	updated.Title = "Edited"
	updated.Questions[0].Text = "Changed"
	call("PUT", "/api/quizzes/"+id, host, updated, 200)
	historyRequest, err := http.NewRequest("GET", server.URL+"/api/games", nil)
	if err != nil {
		t.Fatal(err)
	}
	historyRequest.Header.Set("Authorization", "Bearer "+host)
	historyResponse, err := server.Client().Do(historyRequest)
	if err != nil {
		t.Fatal(err)
	}
	var history []struct {
		Code  string `json:"code"`
		Title string `json:"title"`
	}
	err = json.NewDecoder(historyResponse.Body).Decode(&history)
	historyResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if historyResponse.StatusCode != http.StatusOK || len(history) != 1 || history[0].Code != room || history[0].Title != "Edited" {
		t.Fatalf("game list did not reflect renamed quiz: %+v", history)
	}
	call("DELETE", "/api/quizzes/"+id, host, nil, 200)
	a.mu.Lock()
	saved := cloneRoom(a.rooms[room])
	a.mu.Unlock()
	if saved.Quiz.Title != "Edited" || saved.Quiz.Questions[0].Text != "The red planet?" {
		t.Fatal("quiz rename must update the title while preserving the game questions")
	}
	call("POST", "/api/action", host, map[string]any{"room": room, "action": "leaderboard", "revision": 3}, 200)
	call("POST", "/api/action", host, map[string]any{"room": room, "action": "next", "revision": 4}, 200)
	call("POST", "/api/action", host, map[string]any{"room": room, "action": "finish", "revision": 5}, 200)
	result := call("GET", "/api/games/"+room, host, nil, 200)
	if result["game"].(map[string]any)["phase"] != "finished" {
		t.Fatal("history missing finished game")
	}
	if result["game"].(map[string]any)["quiz"].(map[string]any)["title"] != "Edited" || result["snapshot"].(map[string]any)["title"] != "Edited" {
		t.Fatal("saved results lost the renamed title after quiz deletion")
	}
	call("POST", "/api/host/logout", host, map[string]any{}, 200)
	call("GET", "/api/session", host, nil, 401)
}

func TestConcurrentAnswersCommitOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	a, err := newApp(ctx, s, "password")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	room := Room{Code: "ABCDEFGH", Quiz: testQuiz(), Phase: "question", Revision: 2, Deadline: now + 20000, StartedAt: now, Players: []Player{{ID: "p1", Name: "Ada"}, {ID: "p2", Name: "Lin"}}}
	if err = s.SaveRoom(ctx, room, nil); err != nil {
		t.Fatal(err)
	}
	a.rooms[room.Code] = room
	token, _, err := s.NewSession(ctx, "player", room.Code, "p1")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.routes())
	defer server.Close()
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for range 8 {
		wg.Go(func() {
			req, err := http.NewRequest("POST", server.URL+"/api/action", strings.NewReader(`{"room":"ABCDEFGH","action":"answer","revision":2,"question":0,"choice":0}`))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			res, err := server.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			_, _ = io.Copy(io.Discard, res.Body)
			if res.StatusCode == 200 {
				accepted.Add(1)
			} else if res.StatusCode != 409 {
				t.Errorf("unexpected status %d", res.StatusCode)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d duplicate requests", accepted.Load())
	}
	a.mu.Lock()
	got := a.rooms[room.Code]
	a.mu.Unlock()
	if len(got.Answers) != 1 || got.Players[0].Score > 1000 {
		t.Fatal("duplicate points awarded")
	}
}

func TestMultipleStreamsPresence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	a, err := newApp(ctx, s, "password")
	if err != nil {
		t.Fatal(err)
	}
	r := Room{Code: "ABCDEFGH", Quiz: testQuiz(), Phase: "lobby", Players: []Player{{ID: "p1", Name: "Ada"}}}
	a.rooms[r.Code] = r
	if err = s.SaveRoom(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	token, _, err := s.NewSession(ctx, "player", r.Code, "p1")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.routes())
	defer server.Close()
	defer a.closeStreams()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/live?token=" + token
	first, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.CloseNow()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		for {
			if _, _, err := first.Read(ctx); err != nil {
				return
			}
		}
	}()
	second, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.CloseNow()
	go func() {
		for {
			if _, _, err := second.Read(ctx); err != nil {
				return
			}
		}
	}()
	for _, want := range []int{2, 1, 0} {
		if want == 1 {
			first.CloseNow()
			<-firstDone
		}
		if want == 0 {
			second.CloseNow()
		}
		deadline := time.Now().Add(time.Second)
		for {
			a.mu.Lock()
			count := a.online(r.Code)["p1"]
			a.mu.Unlock()
			if count == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(fmt.Sprintf("presence count = %d, want %d", count, want))
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func TestAPIRejectsInvalidRequests(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	a, err := newApp(ctx, s, "password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.NewSession(ctx, "host", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, method, path, body string
		token                    bool
		want                     int
	}{
		{name: "missing auth", method: "GET", path: "/api/quizzes", want: 401},
		{name: "unknown JSON field", method: "POST", path: "/api/rooms", body: `{"unexpected":true}`, token: true, want: 400},
		{name: "trailing document", method: "POST", path: "/api/rooms", body: `{} {}`, token: true, want: 400},
		{name: "unknown room", method: "POST", path: "/api/join", body: `{"room":"MISSING","name":"Ada"}`, want: 404},
		{name: "unknown API", method: "GET", path: "/api/missing", want: 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.token {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("API response can be cached")
			}
		})
	}
}

func TestCrossOriginRequests(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	a, err := newApp(ctx, s, "password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.NewSession(ctx, "host", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, method, path, origin, body string
		auth                             bool
		want                             int
	}{
		{name: "cloudflared preflight", method: "OPTIONS", path: "/api/quizzes", origin: "https://kahut.trycloudflare.com", want: 204},
		{name: "ngrok authenticated request", method: "GET", path: "/api/session", origin: "https://kahut.ngrok-free.app", auth: true, want: 200},
		{name: "cross-origin login", method: "POST", path: "/api/host/login", origin: "https://kahut.trycloudflare.com", body: `{"password":"password"}`, want: 200},
		{name: "authentication still required", method: "GET", path: "/api/session", origin: "https://kahut.ngrok-free.app", want: 401},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Origin", tt.origin)
			if tt.auth {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			if tt.method == "OPTIONS" {
				req.Header.Set("Access-Control-Request-Method", "POST")
				req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
			}
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Fatal("missing cross-origin access on response")
			}
			if w.Header().Get("Access-Control-Allow-Credentials") != "" {
				t.Fatal("cookie credentials must not be enabled")
			}
			if tt.method == "OPTIONS" {
				if w.Body.Len() != 0 || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Authorization") || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Content-Type") {
					t.Fatal("preflight did not allow bearer authentication and JSON")
				}
				for _, method := range []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"} {
					if !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), method) {
						t.Fatalf("preflight did not allow %s", method)
					}
				}
			}
		})
	}
}

func TestHostDashboardRoute(t *testing.T) {
	s, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	a, err := newApp(context.Background(), s, "password")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		path   string
		status int
	}{
		{"/host", http.StatusOK},
		{"/host/quizzes/new", http.StatusOK},
		{"/host/quizzes/quiz-1/edit", http.StatusOK},
		{"/host/games/ABCDEFGH", http.StatusOK},
		{"/rooms/ABCDEFGH", http.StatusOK},
		{"/resume", http.StatusOK},
		{"/host/", http.StatusPermanentRedirect},
		{"/host/quizzes/quiz-1/edit/", http.StatusPermanentRedirect},
		{"/host/quizzes/quiz-1/unknown", http.StatusNotFound},
		{"/unknown-page", http.StatusNotFound},
	} {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
			}
			if tt.status == http.StatusOK {
				if !strings.Contains(w.Body.String(), `x-data="quizApp"`) {
					t.Fatal("host route did not serve the application shell")
				}
				if w.Header().Get("Cache-Control") != "no-cache" {
					t.Fatal("host shell must allow revalidation")
				}
			}
			if tt.status == http.StatusPermanentRedirect && w.Header().Get("Location") != strings.TrimSuffix(tt.path, "/") {
				t.Fatal("host route did not canonicalize the trailing slash")
			}
		})
	}
}
