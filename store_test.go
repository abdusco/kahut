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
			s, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			token, session, err := s.NewSession(ctx, "player", "ABCDEFGH", "p1")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UnixMilli()
			deadline := now + 60000
			if tt.expired {
				deadline = now - 1000
			}
			r := Room{Code: "ABCDEFGH", Quiz: testQuiz(), Phase: "question", Revision: 2, Sequence: 3, StartedAt: now - 10000, Deadline: deadline, Players: []Player{{ID: "p1", Name: "Ada", Score: 750}, {ID: "p2", Name: "Lin"}}, Answers: []Answer{{PlayerID: "p1", Question: 0, Choice: 0, Points: 750, ReceivedAt: now - 5000}}}
			if err = s.SaveRoom(ctx, r, &r.Answers[0]); err != nil {
				t.Fatal(err)
			}
			if err = s.db.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.db.Close()
			a, err := newApp(ctx, s, "password")
			if err != nil {
				t.Fatal(err)
			}
			got := a.rooms[r.Code]
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
	s, err := openStore(filepath.Join(t.TempDir(), "quiz.db"))
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
	s, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	token, session, err := s.NewSession(ctx, "host", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, token); err != nil {
		t.Fatal(err)
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
