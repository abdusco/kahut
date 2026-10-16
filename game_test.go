package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func testQuiz() Quiz {
	return Quiz{Version: 1, Title: "Space", Questions: []Question{
		{Text: "The red planet?", Choices: []string{"Mars", "Venus"}, CorrectIndex: 0, TimeLimitSeconds: 20},
		{Text: "Gold?", Choices: []string{"Ag", "Au"}, CorrectIndex: 1, TimeLimitSeconds: 10},
	}}
}

func TestScore(t *testing.T) {
	for _, tt := range []struct {
		name    string
		elapsed int64
		want    int
	}{
		{"immediate", 0, 1000}, {"halfway", 10000, 750}, {"last millisecond", 19999, 500},
		{"deadline", 20000, 500}, {"negative elapsed", -1000, 1000}, {"overrun", 21000, 500},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := score(tt.elapsed, 20000); got != tt.want {
				t.Fatalf("score = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestQuizValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Quiz)
		valid  bool
	}{
		{"valid", func(q *Quiz) {}, true},
		{"default timer", func(q *Quiz) { q.Questions[0].TimeLimitSeconds = 0 }, true},
		{"wrong version", func(q *Quiz) { q.Version = 2 }, false},
		{"empty title", func(q *Quiz) { q.Title = " " }, false},
		{"long title", func(q *Quiz) { q.Title = strings.Repeat("ş", 121) }, false},
		{"empty questions", func(q *Quiz) { q.Questions = nil }, false},
		{"empty choice", func(q *Quiz) { q.Questions[0].Choices[0] = " " }, false},
		{"one choice", func(q *Quiz) { q.Questions[0].Choices = []string{"Mars"} }, false},
		{"missing correct choice", func(q *Quiz) { q.Questions[0].CorrectIndex = 2 }, false},
		{"short timer", func(q *Quiz) { q.Questions[0].TimeLimitSeconds = 4 }, false},
		{"long timer", func(q *Quiz) { q.Questions[0].TimeLimitSeconds = 121 }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := testQuiz()
			tt.change(&q)
			if err := q.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate() = %v, valid = %v", err, tt.valid)
			}
			if tt.name == "default timer" && q.Questions[0].TimeLimitSeconds != 20 {
				t.Fatal("timer was not defaulted")
			}
		})
	}
}

func TestAnswerRules(t *testing.T) {
	for _, tt := range []struct {
		name             string
		player           string
		question, choice int
		revision, now    int64
		phase            string
		from             int
		existing         bool
		valid            bool
		points           int
	}{
		{name: "correct", player: "p1", revision: 2, now: 11000, phase: "question", valid: true, points: 750},
		{name: "wrong", player: "p1", choice: 1, revision: 2, now: 11000, phase: "question", valid: true},
		{name: "unknown player", player: "missing", revision: 2, now: 11000, phase: "question"},
		{name: "late join", player: "p1", revision: 2, now: 11000, phase: "question", from: 1},
		{name: "duplicate", player: "p1", revision: 2, now: 11000, phase: "question", existing: true},
		{name: "stale revision", player: "p1", revision: 1, now: 11000, phase: "question"},
		{name: "stale question", player: "p1", question: 1, revision: 2, now: 11000, phase: "question"},
		{name: "negative choice", player: "p1", choice: -1, revision: 2, now: 11000, phase: "question"},
		{name: "invalid choice", player: "p1", choice: 4, revision: 2, now: 11000, phase: "question"},
		{name: "at deadline", player: "p1", revision: 2, now: 21000, phase: "question"},
		{name: "after deadline", player: "p1", revision: 2, now: 21001, phase: "question"},
		{name: "wrong phase", player: "p1", revision: 2, now: 11000, phase: "results"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := Room{Quiz: testQuiz(), Phase: tt.phase, Revision: 2, StartedAt: 1000, Deadline: 21000, Players: []Player{{ID: "p1", FromQuestion: tt.from}, {ID: "p2"}}}
			if tt.existing {
				r.Answers = []Answer{{PlayerID: "p1", Question: 0}}
			}
			before, _ := json.Marshal(r)
			a, err := r.Submit(tt.player, tt.question, tt.choice, tt.revision, tt.now)
			if (err == nil) != tt.valid {
				t.Fatalf("Submit() = %v, valid = %v", err, tt.valid)
			}
			if tt.valid {
				if a.Points != tt.points || r.Players[0].Score != tt.points {
					t.Fatalf("wrong score: %+v", a)
				}
				if r.Revision != 2 {
					t.Fatal("one answer must not invalidate another player's revision")
				}
			} else {
				after, _ := json.Marshal(r)
				if string(before) != string(after) {
					t.Fatal("invalid answer mutated game")
				}
			}
		})
	}
}

func TestGameProgression(t *testing.T) {
	r := Room{Quiz: testQuiz(), Phase: "lobby", Revision: 1}
	if err := r.HostAction("start", 1, 1000); err == nil {
		t.Fatal("started without players")
	}
	if err := r.Join("p1", "Ada"); err != nil {
		t.Fatal(err)
	}
	if err := r.HostAction("start", 1, 1000); err != nil {
		t.Fatal(err)
	}
	if r.Deadline != 21000 {
		t.Fatal("wrong deadline")
	}
	if err := r.Join("p2", "Lin"); err != nil {
		t.Fatal(err)
	}
	if r.Players[1].FromQuestion != 1 {
		t.Fatal("late join was eligible")
	}
	if _, err := r.Submit("p1", 0, 0, r.Revision, 2000); err != nil {
		t.Fatal(err)
	}
	if r.Phase != "results" {
		t.Fatal("all eligible answers did not close question")
	}
	if err := r.HostAction("next", r.Revision, 3000); err == nil {
		t.Fatal("skipped leaderboard")
	}
	if err := r.HostAction("leaderboard", r.Revision, 3000); err != nil {
		t.Fatal(err)
	}
	if err := r.HostAction("next", r.Revision, 4000); err != nil {
		t.Fatal(err)
	}
	if r.Question != 1 || r.Deadline != 14000 {
		t.Fatal("wrong second question")
	}
	if _, err := r.Submit("p2", 1, 1, r.Revision, 5000); err != nil {
		t.Fatal(err)
	}
	if r.Phase != "question" {
		t.Fatal("question ended before other eligible player answered")
	}
	if !r.CloseExpired(14000) || r.CloseExpired(14001) {
		t.Fatal("deadline was not applied exactly once")
	}
	if err := r.HostAction("leaderboard", r.Revision, 15000); err != nil {
		t.Fatal(err)
	}
	if err := r.HostAction("next", r.Revision, 15000); err != nil {
		t.Fatal(err)
	}
	if r.Phase != "finished" {
		t.Fatal("game did not finish")
	}
	if err := r.Join("p3", "Sam"); err == nil {
		t.Fatal("joined finished game")
	}
}

func TestJoinValidation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		nickname string
		full     bool
		want     bool
	}{
		{"valid", "  Lin  ", false, true}, {"case duplicate", "aDA", false, false}, {"blank", " ", false, false}, {"long", strings.Repeat("a", 25), false, false}, {"line break", "a\nb", false, false}, {"full", "Lin", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := Room{Phase: "lobby", Players: []Player{{ID: "p1", Name: "Ada"}}}
			if tt.full {
				for i := 1; i < 100; i++ {
					r.Players = append(r.Players, Player{})
				}
			}
			if err := r.Join("p2", tt.nickname); (err == nil) != tt.want {
				t.Fatalf("Join() = %v", err)
			}
			if tt.want && r.Players[1].Name != "Lin" {
				t.Fatal("nickname not trimmed")
			}
		})
	}
}

func TestSnapshotPrivacyAndTies(t *testing.T) {
	r := Room{Code: "ABCDEFGH", Quiz: testQuiz(), Phase: "question", Revision: 2, StartedAt: 1000, Deadline: 21000, Players: []Player{{ID: "p1", Name: "Ada"}, {ID: "p2", Name: "Lin"}, {ID: "p3", Name: "Sam"}}}
	if _, err := r.Submit("p1", 0, 0, 2, 11000); err != nil {
		t.Fatal(err)
	}
	for _, host := range []bool{false, true} {
		s := r.Snapshot("p1", host, map[string]int{"p1": 2}, 12000)
		b, _ := json.Marshal(s)
		for _, forbidden := range []string{"correctIndex", "Gold?", "myPoints", "receivedAt", "playerId\":\"p2"} {
			if strings.Contains(string(b), forbidden) {
				t.Fatalf("private data leaked: %s", b)
			}
		}
		leaders := s["players"].([]Standing)
		if leaders[0].Score != 0 || leaders[1].Score != 0 || leaders[0].Rank != 1 || leaders[1].Rank != 1 {
			t.Fatal("active score leaked correctness or broke ties")
		}
	}
	r.Phase = "results"
	s := r.Snapshot("p1", false, map[string]int{}, 22000)
	q := s["question"].(map[string]any)
	if q["correctIndex"] != 0 || q["counts"].([]int)[0] != 1 || s["myPoints"] != 750 {
		t.Fatal("results incomplete")
	}
	leaders := s["players"].([]Standing)
	if leaders[0].Rank != 1 || leaders[1].Rank != 2 || leaders[2].Rank != 2 {
		t.Fatal("tie ranks are not shared")
	}
}
