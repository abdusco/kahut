package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestAccountsOwnQuizzesAndGames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := openRepository(":memory:", "test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := newApp(ctx, s, `["alice:secret","bob:other"]`)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.routes())
	defer server.Close()
	defer a.closeStreams()
	call := func(method, path, token string, body any, want int) []byte {
		t.Helper()
		data, _ := json.Marshal(body)
		request, err := http.NewRequest(method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, want, result)
		}
		return result
	}
	alice, _, err := s.NewSession(ctx, "host", "", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.NewSession(ctx, "host", "", "", "bob")
	if err != nil {
		t.Fatal(err)
	}
	var aliceQuiz, bobQuiz Quiz
	if err = json.Unmarshal(call("POST", "/api/quizzes", alice, testQuiz(), 200), &aliceQuiz); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(call("POST", "/api/quizzes/import", bob, testQuiz(), 200), &bobQuiz); err != nil {
		t.Fatal(err)
	}
	var aliceRoom, bobRoom struct {
		Room string `json:"room"`
	}
	if err = json.Unmarshal(call("POST", "/api/rooms", alice, map[string]string{"quizId": aliceQuiz.ID}, 200), &aliceRoom); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(call("POST", "/api/rooms", bob, map[string]string{"quizId": bobQuiz.ID}, 200), &bobRoom); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, token, otherToken, quizID, otherQuizID, room string
	}{
		{"alice", alice, bob, aliceQuiz.ID, bobQuiz.ID, aliceRoom.Room},
		{"bob", bob, alice, bobQuiz.ID, aliceQuiz.ID, bobRoom.Room},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var quizzes []Quiz
			if err := json.Unmarshal(call("GET", "/api/quizzes", tt.token, nil, 200), &quizzes); err != nil || len(quizzes) != 1 || quizzes[0].ID != tt.quizID {
				t.Fatalf("quiz list crossed account boundary: %+v, %v", quizzes, err)
			}
			var games []struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(call("GET", "/api/games", tt.token, nil, 200), &games); err != nil || len(games) != 1 || games[0].Code != tt.room {
				t.Fatalf("game list crossed account boundary: %+v, %v", games, err)
			}
			for _, request := range []struct {
				method, path string
				body         any
			}{
				{"GET", "/api/quizzes/" + tt.otherQuizID, nil},
				{"GET", "/api/quizzes/" + tt.otherQuizID + "/export", nil},
				{"PUT", "/api/quizzes/" + tt.otherQuizID, testQuiz()},
				{"DELETE", "/api/quizzes/" + tt.otherQuizID, nil},
				{"POST", "/api/rooms", map[string]string{"quizId": tt.otherQuizID}},
			} {
				call(request.method, request.path, tt.token, request.body, 404)
			}
			call("POST", "/api/action", tt.otherToken, map[string]any{"room": tt.room, "action": "finish", "revision": 1}, 404)
			call("GET", "/api/games/"+tt.room, tt.otherToken, nil, 404)
			dialCtx, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			conn, response, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/live?token="+tt.otherToken+"&room="+tt.room, nil)
			if conn != nil {
				conn.CloseNow()
				t.Fatal("another account connected to a host's room")
			}
			if err == nil || response == nil || response.StatusCode != 404 {
				t.Fatalf("cross-account WebSocket = %v, %v", response, err)
			}
			call("POST", "/api/action", tt.token, map[string]any{"room": tt.room, "action": "finish", "revision": 1}, 200)
			call("GET", "/api/games/"+tt.room, tt.token, nil, 200)
			call("GET", "/api/games/"+tt.room, tt.otherToken, nil, 404)
			exported := call("GET", "/api/quizzes/"+tt.quizID+"/export", tt.token, nil, 200)
			if bytes.Contains(exported, []byte(`"owner"`)) {
				t.Fatal("portable export included account metadata")
			}
		})
	}
	// Removing an account from configuration also rejects its saved bearer token.
	reconfigured, err := newApp(ctx, s, `["bob:other"]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/session", "/api/live?token=" + alice + "&room=" + aliceRoom.Room} {
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+alice)
		response := httptest.NewRecorder()
		reconfigured.routes().ServeHTTP(response, request)
		if response.Code != 401 {
			t.Fatalf("removed account accepted at %s: %d", path, response.Code)
		}
	}
}
