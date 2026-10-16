package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// tinyPNG is a valid 1x1 PNG.
var tinyPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
	0, 0, 0, 0x0d, 'I', 'D', 'A', 'T', 0x78, 0x9c, 0x63, 0xf8, 0xff, 0xff, 0x3f, 0, 5, 0xfe, 2, 0xfe, 0xa7, 0x35, 0x81, 0x84,
	0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

func TestImageUploadAndQuestions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo, err := openRepository(":memory:", "test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	a, err := newApp(ctx, repo, `["alice:secret","bob:other"]`)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.routes())
	defer server.Close()
	defer a.closeStreams()
	alice, _, _ := repo.NewSession(ctx, "host", "", "", "alice")
	bob, _, _ := repo.NewSession(ctx, "host", "", "", "bob")
	do := func(method, path, token, contentType string, body []byte, want int) []byte {
		t.Helper()
		request, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", contentType)
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
	upload := func(token string, data []byte, want int) string {
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(do("POST", "/api/images", token, "image/png", data, want), &out)
		return out.ID
	}

	upload("", tinyPNG, 401)
	upload(alice, []byte("<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>"), 415)
	upload(alice, []byte("plain text"), 415)
	upload(alice, append(append([]byte{}, tinyPNG...), make([]byte, maxImageBytes)...), 413)
	id := upload(alice, tinyPNG, 200)

	// Served without authentication, with safe headers.
	response, err := server.Client().Get(server.URL + "/api/images/" + id)
	if err != nil {
		t.Fatal(err)
	}
	served, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "image/png" || !bytes.Equal(served, tinyPNG) {
		t.Fatalf("unexpected image response %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("image response is missing hardening headers")
	}
	do("GET", "/api/images/unknown", "", "", nil, 404)

	quiz := testQuiz()
	quiz.Questions[0].ImageID = id
	body, _ := json.Marshal(quiz)
	do("POST", "/api/quizzes", bob, "application/json", body, 400) // not bob's image
	saved := do("POST", "/api/quizzes", alice, "application/json", body, 200)
	var created Quiz
	if err = json.Unmarshal(saved, &created); err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.Quiz(ctx, created.ID, "alice")
	if err != nil || loaded.Questions[0].ImageID != id {
		t.Fatalf("image not stored with question: %+v, %v", loaded.Questions, err)
	}
	var exported Quiz
	_ = json.Unmarshal(do("GET", "/api/quizzes/"+created.ID+"/export", alice, "", nil, 200), &exported)
	if exported.Questions[0].ImageID != "" {
		t.Fatal("export included image ID")
	}

	// A game keeps its image after the quiz is edited or deleted, and the
	// image is pruned once nothing references it.
	var room struct {
		Room string `json:"room"`
	}
	_ = json.Unmarshal(do("POST", "/api/rooms", alice, "application/json", mustJSON(map[string]string{"quizId": created.ID}), 200), &room)
	quiz.Questions[0].ImageID = ""
	body, _ = json.Marshal(quiz)
	do("PUT", "/api/quizzes/"+created.ID, alice, "application/json", body, 200)
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	if _, err = repo.db.Exec("UPDATE images SET created_at=?", old); err != nil {
		t.Fatal(err)
	}
	do("DELETE", "/api/quizzes/"+created.ID, alice, "application/json", nil, 200)
	do("GET", "/api/images/"+id, "", "", nil, 200)
	rooms, err := repo.Rooms(ctx)
	if err != nil || len(rooms) != 1 || rooms[0].Quiz.Questions[0].ImageID != id {
		t.Fatalf("room lost its image: %+v, %v", rooms, err)
	}

	orphan := upload(alice, tinyPNG, 200)
	if _, err = repo.db.Exec("UPDATE images SET created_at=? WHERE id=?", old, orphan); err != nil {
		t.Fatal(err)
	}
	repo.pruneImages(ctx)
	do("GET", "/api/images/"+orphan, "", "", nil, 404)
	do("GET", "/api/images/"+id, "", "", nil, 200)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
