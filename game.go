package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

type Question struct {
	Text             string   `json:"text"`
	Choices          []string `json:"choices"`
	CorrectIndex     int      `json:"correctIndex"`
	TimeLimitSeconds int      `json:"timeLimitSeconds"`
}

type Quiz struct {
	ID        string     `json:"id,omitempty"`
	Version   int        `json:"version"`
	Title     string     `json:"title"`
	Questions []Question `json:"questions"`
}

func (q *Quiz) Validate() error {
	q.Title = strings.TrimSpace(q.Title)
	if q.Version != 1 {
		return errors.New("quiz version must be 1")
	}
	if q.Title == "" || utf8.RuneCountInString(q.Title) > 120 {
		return errors.New("title must contain 1–120 characters")
	}
	if len(q.Questions) < 1 || len(q.Questions) > 100 {
		return errors.New("quiz must contain 1–100 questions")
	}
	for i := range q.Questions {
		p := &q.Questions[i]
		p.Text = strings.TrimSpace(p.Text)
		if p.Text == "" || utf8.RuneCountInString(p.Text) > 1000 {
			return fmt.Errorf("question %d: text must contain 1–1000 characters", i+1)
		}
		if len(p.Choices) < 2 || len(p.Choices) > 4 || p.CorrectIndex < 0 || p.CorrectIndex >= len(p.Choices) {
			return fmt.Errorf("question %d: provide 2–4 choices and one correct index", i+1)
		}
		for j, c := range p.Choices {
			p.Choices[j] = strings.TrimSpace(c)
			if p.Choices[j] == "" || utf8.RuneCountInString(p.Choices[j]) > 300 {
				return fmt.Errorf("question %d: choices must contain 1–300 characters", i+1)
			}
		}
		if p.TimeLimitSeconds == 0 {
			p.TimeLimitSeconds = 20
		}
		if p.TimeLimitSeconds < 5 || p.TimeLimitSeconds > 120 {
			return fmt.Errorf("question %d: timer must be 5–120 seconds", i+1)
		}
	}
	return nil
}

type Player struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Score        int    `json:"score"`
	FromQuestion int    `json:"fromQuestion"`
}

type Answer struct {
	PlayerID   string `json:"playerId"`
	Question   int    `json:"question"`
	Choice     int    `json:"choice"`
	Points     int    `json:"points"`
	ReceivedAt int64  `json:"receivedAt"`
}

type Room struct {
	Code      string   `json:"code"`
	Quiz      Quiz     `json:"quiz"`
	Phase     string   `json:"phase"`
	Question  int      `json:"question"`
	Revision  int64    `json:"revision"`
	Sequence  int64    `json:"sequence"`
	StartedAt int64    `json:"startedAt"`
	Deadline  int64    `json:"deadline"`
	CreatedAt int64    `json:"createdAt"`
	Players   []Player `json:"players"`
	Answers   []Answer `json:"answers"`
}

func (r Room) Player(id string) *Player {
	for i := range r.Players {
		if r.Players[i].ID == id {
			return &r.Players[i]
		}
	}
	return nil
}

func (r Room) Answer(player string, question int) *Answer {
	for i := range r.Answers {
		if r.Answers[i].PlayerID == player && r.Answers[i].Question == question {
			return &r.Answers[i]
		}
	}
	return nil
}

func (r *Room) Join(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 24 || strings.ContainsAny(name, "\r\n\t") {
		return errors.New("nickname must contain 1–24 characters")
	}
	if r.Phase == "finished" {
		return errors.New("game has finished")
	}
	if len(r.Players) >= 100 {
		return errors.New("room is full")
	}
	for _, p := range r.Players {
		if strings.EqualFold(p.Name, name) {
			return errors.New("nickname is already taken")
		}
	}
	from := r.Question
	if r.Phase != "lobby" {
		from++
	}
	r.Players = append(r.Players, Player{ID: id, Name: name, FromQuestion: from})
	return nil
}

func score(elapsed, duration int64) int {
	fraction := math.Max(0, math.Min(1, float64(elapsed)/float64(duration)))
	return int(math.Round(1000 - 500*fraction))
}

func (r *Room) CloseExpired(now int64) bool {
	if r.Phase == "question" && now >= r.Deadline {
		r.Phase = "results"
		r.Revision++
		return true
	}
	return false
}

func (r *Room) Submit(player string, question, choice int, revision, now int64) (*Answer, error) {
	if r.Phase != "question" || now >= r.Deadline {
		return nil, errors.New("answering is closed")
	}
	if revision != r.Revision || question != r.Question {
		return nil, errors.New("stale question; refresh live state")
	}
	p := r.Player(player)
	if p == nil || p.FromQuestion > r.Question {
		return nil, errors.New("wait for the next question")
	}
	if r.Answer(player, question) != nil {
		return nil, errors.New("answer already submitted")
	}
	q := r.Quiz.Questions[r.Question]
	if choice < 0 || choice >= len(q.Choices) {
		return nil, errors.New("invalid choice")
	}
	a := Answer{PlayerID: player, Question: question, Choice: choice, ReceivedAt: now}
	if choice == q.CorrectIndex {
		a.Points = score(now-r.StartedAt, r.Deadline-r.StartedAt)
	}
	p.Score += a.Points
	r.Answers = append(r.Answers, a)
	all := true
	for _, p := range r.Players {
		if p.FromQuestion <= r.Question && r.Answer(p.ID, r.Question) == nil {
			all = false
			break
		}
	}
	if all {
		r.Phase = "results"
		r.Revision++
	}
	return &a, nil
}

func (r *Room) HostAction(action string, revision, now int64) error {
	if revision != r.Revision {
		return errors.New("stale game state")
	}
	switch action {
	case "start", "next":
		if action == "start" && (r.Phase != "lobby" || len(r.Players) == 0) {
			return errors.New("start requires a lobby with players")
		}
		if action == "next" {
			if r.Phase != "leaderboard" {
				return errors.New("show the leaderboard first")
			}
			if r.Question+1 >= len(r.Quiz.Questions) {
				r.Phase = "finished"
				r.Revision++
				return nil
			}
			r.Question++
		}
		r.Phase = "question"
		r.StartedAt = now
		r.Deadline = now + int64(r.Quiz.Questions[r.Question].TimeLimitSeconds)*1000
	case "close":
		if r.Phase != "question" {
			return errors.New("no active question")
		}
		r.Phase = "results"
	case "leaderboard":
		if r.Phase != "results" {
			return errors.New("results are not ready")
		}
		r.Phase = "leaderboard"
	case "finish":
		if r.Phase == "finished" {
			return errors.New("game has finished")
		}
		r.Phase = "finished"
	default:
		return errors.New("unknown host action")
	}
	r.Revision++
	return nil
}

type Standing struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Score    int    `json:"score"`
	Rank     int    `json:"rank"`
	Online   bool   `json:"online"`
	Answered bool   `json:"answered"`
}

func (r Room) Snapshot(player string, host bool, online map[string]int, now int64) map[string]any {
	leaders := make([]Standing, 0, len(r.Players))
	for _, p := range r.Players {
		// Keep scores from revealing correctness while a question is open.
		s := p.Score
		if r.Phase == "question" {
			if a := r.Answer(p.ID, r.Question); a != nil {
				s -= a.Points
			}
		}
		leaders = append(leaders, Standing{ID: p.ID, Name: p.Name, Score: s, Online: online[p.ID] > 0, Answered: r.Answer(p.ID, r.Question) != nil})
	}
	sort.SliceStable(leaders, func(i, j int) bool { return leaders[i].Score > leaders[j].Score })
	for i := range leaders {
		if i > 0 && leaders[i].Score == leaders[i-1].Score {
			leaders[i].Rank = leaders[i-1].Rank
		} else {
			leaders[i].Rank = i + 1
		}
	}
	s := map[string]any{"code": r.Code, "title": r.Quiz.Title, "phase": r.Phase, "questionIndex": r.Question, "questionCount": len(r.Quiz.Questions), "revision": r.Revision, "sequence": r.Sequence, "serverTime": now, "deadline": r.Deadline, "players": leaders, "host": host, "playerId": player, "eligible": false, "answered": false}
	eligibleCount, answeredCount := 0, 0
	for _, p := range r.Players {
		if p.FromQuestion <= r.Question {
			eligibleCount++
			if r.Answer(p.ID, r.Question) != nil {
				answeredCount++
			}
		}
	}
	s["eligibleCount"] = eligibleCount
	s["answeredCount"] = answeredCount
	if p := r.Player(player); p != nil {
		s["eligible"] = p.FromQuestion <= r.Question
	}
	if r.Phase != "lobby" {
		q := r.Quiz.Questions[r.Question]
		view := map[string]any{"text": q.Text, "choices": q.Choices, "timeLimitSeconds": q.TimeLimitSeconds}
		if r.Phase != "question" {
			view["correctIndex"] = q.CorrectIndex
			counts := make([]int, len(q.Choices))
			for _, a := range r.Answers {
				if a.Question == r.Question {
					counts[a.Choice]++
				}
			}
			view["counts"] = counts
		}
		s["question"] = view
		if a := r.Answer(player, r.Question); a != nil {
			s["answered"] = true
			s["myChoice"] = a.Choice
			if r.Phase != "question" {
				s["myPoints"] = a.Points
			}
		}
	}
	return s
}
