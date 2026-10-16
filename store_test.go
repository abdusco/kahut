package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRestartRecovery(t *testing.T) {
	for _, tt := range []struct {
		name    string
		expired bool
		phase   string
	}{
		{"remaining time", false, "question"}, {"elapsed deadline", true, "results"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "quiz.db")
			s, err := openStore(path, "test-signing-key-at-least-32-bytes")
			if err != nil {
				t.Fatal(err)
			}
			token, session, err := s.NewSession(ctx, "player", "ABCDEFGH", "p1", "")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UnixMilli()
			deadline := now + 60000
			if tt.expired {
				deadline = now - 1000
			}
			r := Room{Owner: "host", Code: "ABCDEFGH", Quiz: testQuiz(), Phase: "question", Revision: 2, Sequence: 3, StartedAt: now - 10000, Deadline: deadline, Players: []Player{{ID: "p1", Name: "Ada", Score: 750}, {ID: "p2", Name: "Lin"}}, Answers: []Answer{{PlayerID: "p1", Question: 0, Choice: 0, Points: 750, ReceivedAt: now - 5000}}}
			if err = s.SaveRoom(ctx, r, &r.Answers[0]); err != nil {
				t.Fatal(err)
			}
			if err = s.db.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = openStore(path, "test-signing-key-at-least-32-bytes")
			if err != nil {
				t.Fatal(err)
			}
			defer s.db.Close()
			a, err := newApp(ctx, s, `["host:password"]`)
			if err != nil {
				t.Fatal(err)
			}
			got := a.rooms[r.Code]
			if got.Owner != "host" || cloneRoom(got).Owner != "host" {
				t.Fatal("room ownership was lost during persistence or cloning")
			}
			if got.Phase != tt.phase || got.Deadline != deadline || got.Players[0].Score != 750 || len(got.Answers) != 1 {
				t.Fatalf("bad recovery: %+v", got)
			}
			if got.CloseExpired(time.Now().UnixMilli()) && tt.expired {
				t.Fatal("expired question closed twice")
			}
			restored, err := s.Session(ctx, token)
			if err != nil || restored.ID != session.ID {
				t.Fatalf("session did not survive restart: %v", err)
			}
			if _, err = got.Submit("p1", 0, 0, got.Revision, now); err == nil {
				t.Fatal("accepted duplicate after restart")
			}
		})
	}
}

func TestRoomAnswerTransactionRollback(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "quiz.db"), "test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	r := Room{Code: "ABCDEFGH", Quiz: testQuiz(), Phase: "question", Players: []Player{{ID: "p1", Score: 750}}, Answers: []Answer{{PlayerID: "p1", Question: 0, Choice: 0, Points: 750}}}
	if err = s.SaveRoom(ctx, r, &r.Answers[0]); err != nil {
		t.Fatal(err)
	}
	r.Players[0].Score = 1500
	if err = s.SaveRoom(ctx, r, &r.Answers[0]); err == nil {
		t.Fatal("duplicate database answer accepted")
	}
	rooms, err := s.Rooms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rooms[0].Players[0].Score != 750 {
		t.Fatal("room score was committed despite answer constraint failure")
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(":memory:", "test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	token, session, err := s.NewSession(ctx, "host", "", "", "host")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Session(ctx, token); err != nil || got.Username != "host" {
		t.Fatalf("host session lost its account: %+v, %v", got, err)
	}
	if _, err = s.Session(ctx, token+"tampered"); err == nil {
		t.Fatal("tampered token accepted")
	}
	if _, err = s.db.Exec("UPDATE sessions SET expires=? WHERE id=?", time.Now().Add(-time.Second).UnixMilli(), session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, token); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestSigningKeyConfiguration(t *testing.T) {
	for _, key := range []string{"", "too-short"} {
		if s, err := openStore(":memory:", key); err == nil {
			s.db.Close()
			t.Fatal("accepted a missing or short signing key")
		}
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "quiz.db")
	s, err := openStore(path, "first-test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.NewSession(ctx, "host", "", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	quiz := testQuiz()
	quiz.ID, quiz.Owner = "alice-quiz", "alice"
	if err = s.SaveQuiz(ctx, quiz, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path, "first-test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	if session, err := s.Session(ctx, token); err != nil || session.Username != "alice" {
		t.Fatalf("stable signing key did not restore the host session: %+v, %v", session, err)
	}
	if quiz, err := s.OwnedQuiz(ctx, quiz.ID, "alice"); err != nil || quiz.Owner != "alice" {
		t.Fatalf("quiz ownership did not survive restart: %+v, %v", quiz, err)
	}
	var metaTables int
	if err = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='meta'").Scan(&metaTables); err != nil || metaTables != 0 {
		t.Fatalf("unexpected signing-key storage: %d, %v", metaTables, err)
	}
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path, "different-test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if _, err = s.Session(ctx, token); err == nil {
		t.Fatal("rotating the signing key did not invalidate the old token")
	}
}

func TestQuizRenameTransaction(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "quiz.db")
	s, err := openStore(path, "test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	quiz := testQuiz()
	quiz.ID = "quiz-1"
	if err = s.SaveQuiz(ctx, quiz, nil); err != nil {
		t.Fatal(err)
	}
	room := Room{Code: "ABCDEFGH", Quiz: testQuiz(), Phase: "finished", Players: []Player{{ID: "p1", Score: 750}}, Answers: []Answer{{PlayerID: "p1", Question: 0, Choice: 0, Points: 750}}}
	room.Quiz.ID = quiz.ID
	if err = s.SaveRoom(ctx, room, &room.Answers[0]); err != nil {
		t.Fatal(err)
	}
	quiz.Title = "New title"
	quiz.Questions[0].Text = "A different question"
	room.Quiz.Title = quiz.Title
	if _, err = s.db.Exec(`CREATE TRIGGER fail_room_update BEFORE UPDATE ON rooms BEGIN SELECT RAISE(FAIL, 'simulated failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveQuiz(ctx, quiz, []Room{room}); err == nil {
		t.Fatal("expected a room update failure")
	}
	unchanged, err := s.Quiz(ctx, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Title != "Space" {
		t.Fatal("quiz title was committed despite a failed game update")
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_room_update"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveQuiz(ctx, quiz, []Room{room}); err != nil {
		t.Fatal(err)
	}
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path, "test-signing-key-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	a, err := newApp(ctx, s, `["host:password"]`)
	if err != nil {
		t.Fatal(err)
	}
	restored := a.rooms[room.Code]
	if restored.Quiz.Title != "New title" {
		t.Fatal("renamed game title was not preserved after restart")
	}
	if restored.Quiz.Questions[0].Text != "The red planet?" || restored.Players[0].Score != 750 || len(restored.Answers) != 1 {
		t.Fatal("renaming altered historical questions, answers, or scores")
	}
}

func TestExistingGameTitlesFollowSavedQuizOnStartup(t *testing.T) {
	for _, tt := range []struct {
		name         string
		sourceExists bool
		want         string
	}{
		{"renamed quiz", true, "Current title"},
		{"deleted quiz", false, "Original title"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, err := openStore(":memory:", "test-signing-key-at-least-32-bytes")
			if err != nil {
				t.Fatal(err)
			}
			defer s.db.Close()
			quiz := testQuiz()
			quiz.ID, quiz.Title = "quiz-1", "Current title"
			if tt.sourceExists {
				if err = s.SaveQuiz(ctx, quiz, nil); err != nil {
					t.Fatal(err)
				}
			}
			room := Room{Code: "ABCDEFGH", Quiz: testQuiz(), Phase: "finished"}
			room.Quiz.ID, room.Quiz.Title = quiz.ID, "Original title"
			if err = s.SaveRoom(ctx, room, nil); err != nil {
				t.Fatal(err)
			}
			a, err := newApp(ctx, s, `["host:password"]`)
			if err != nil {
				t.Fatal(err)
			}
			if a.rooms[room.Code].Quiz.Title != tt.want {
				t.Fatalf("title = %q, want %q", a.rooms[room.Code].Quiz.Title, tt.want)
			}
			persisted, err := s.Rooms(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if persisted[0].Quiz.Title != tt.want || persisted[0].Quiz.Questions[0].Text != "The red planet?" {
				t.Fatal("startup repair did not preserve the title and original questions")
			}
		})
	}
}
