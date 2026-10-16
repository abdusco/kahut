package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type stream struct {
	session Session
	room    string
	conn    *websocket.Conn
	updates chan []byte
}
type rateWindow struct {
	until time.Time
	count int
}
type App struct {
	ctx      context.Context
	store    *Store
	password string
	mu       sync.Mutex
	rooms    map[string]Room
	streams  map[*stream]bool
	rates    map[string]rateWindow
}

func newApp(ctx context.Context, s *Store, password string) (*App, error) {
	a := &App{ctx: ctx, store: s, password: password, rooms: map[string]Room{}, streams: map[*stream]bool{}, rates: map[string]rateWindow{}}
	rooms, err := s.Rooms(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rooms {
		if r.CloseExpired(time.Now().UnixMilli()) {
			r.Sequence++
			if err = s.SaveRoom(ctx, r, nil); err != nil {
				return nil, err
			}
		}
		a.rooms[r.Code] = r
	}
	return a, nil
}

func cloneRoom(r Room) Room { b, _ := json.Marshal(r); var c Room; _ = json.Unmarshal(b, &c); return c }

func (a *App) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/host/login", a.login)
	m.HandleFunc("POST /api/host/logout", a.logout)
	m.HandleFunc("GET /api/session", a.session)
	m.HandleFunc("GET /api/live", a.live)
	m.HandleFunc("GET /api/quizzes", a.quizzes)
	m.HandleFunc("POST /api/quizzes", a.saveQuiz)
	m.HandleFunc("POST /api/quizzes/import", a.saveQuiz)
	m.HandleFunc("GET /api/quizzes/{id}", a.getQuiz)
	m.HandleFunc("PUT /api/quizzes/{id}", a.saveQuiz)
	m.HandleFunc("DELETE /api/quizzes/{id}", a.deleteQuiz)
	m.HandleFunc("GET /api/quizzes/{id}/export", a.exportQuiz)
	m.HandleFunc("POST /api/rooms", a.createRoom)
	m.HandleFunc("POST /api/join", a.join)
	m.HandleFunc("POST /api/action", a.action)
	m.HandleFunc("GET /api/games", a.games)
	m.HandleFunc("GET /api/games/{id}", a.game)
	var files fs.FS
	if os.Getenv("DEBUG") == "1" {
		files = os.DirFS("assets")
	} else {
		files, _ = fs.Sub(assets, "assets")
	}
	static := http.FileServer(http.FS(files))
	m.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/host/" {
			http.Redirect(w, r, "/host", http.StatusPermanentRedirect)
			return
		}
		if r.URL.Path == "/host" {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFileFS(w, r, files, "index.html")
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/sw.js" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		static.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			// Sessions use explicit bearer tokens, not ambient cookies. Allow
			// browser clients through tunnels even when the proxy rewrites Host.
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeError(w, 400, "expected one JSON document")
		return false
	}
	return true
}
func (a *App) internal(w http.ResponseWriter, err error) {
	log.Printf("storage operation failed: %v", err)
	writeError(w, 500, "storage operation failed; please retry")
}
func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
func (a *App) authenticate(w http.ResponseWriter, r *http.Request, host bool) (Session, bool) {
	s, err := a.store.Session(r.Context(), bearer(r))
	if err != nil {
		writeError(w, 401, "session expired; sign in or join again")
		return s, false
	}
	if host && s.Role != "host" {
		writeError(w, 403, "host access required")
		return s, false
	}
	return s, true
}
func (a *App) limited(r *http.Request, bucket string, max int) bool {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	key := bucket + ":" + ip
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	v := a.rates[key]
	if now.After(v.until) {
		v = rateWindow{until: now.Add(time.Minute)}
	}
	v.count++
	a.rates[key] = v
	if len(a.rates) > 10000 {
		for k, v := range a.rates {
			if now.After(v.until) {
				delete(a.rates, k)
			}
		}
	}
	return v.count > max
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if a.limited(r, "login", 10) {
		writeError(w, 429, "too many attempts; retry in a minute")
		return
	}
	var b struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &b) {
		return
	}
	x, y := sha256.Sum256([]byte(b.Password)), sha256.Sum256([]byte(a.password))
	if subtle.ConstantTimeCompare(x[:], y[:]) != 1 {
		writeError(w, 401, "incorrect host password")
		return
	}
	token, s, err := a.store.NewSession(r.Context(), "host", "", "")
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"token": token, "session": s})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authenticate(w, r, true)
	if !ok {
		return
	}
	if _, err := a.store.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE id=?", s.ID); err != nil {
		a.internal(w, err)
		return
	}
	a.mu.Lock()
	for st := range a.streams {
		if st.session.ID == s.ID {
			st.conn.CloseNow()
		}
	}
	a.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) session(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authenticate(w, r, false)
	if ok {
		writeJSON(w, 200, s)
	}
}
func (a *App) quizzes(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(w, r, true); !ok {
		return
	}
	q, err := a.store.Quizzes(r.Context())
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, 200, q)
}
func (a *App) saveQuiz(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(w, r, true); !ok {
		return
	}
	var q Quiz
	if !decode(w, r, &q) {
		return
	}
	if err := q.Validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if id := r.PathValue("id"); id != "" {
		if _, err := a.store.Quiz(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "quiz not found")
			return
		} else if err != nil {
			a.internal(w, err)
			return
		}
		q.ID = id
	} else {
		q.ID = randomID()
	}
	if err := a.store.SaveQuiz(r.Context(), q); err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, 200, q)
}
func (a *App) getQuiz(w http.ResponseWriter, r *http.Request)    { a.readQuiz(w, r, false) }
func (a *App) exportQuiz(w http.ResponseWriter, r *http.Request) { a.readQuiz(w, r, true) }
func (a *App) readQuiz(w http.ResponseWriter, r *http.Request, export bool) {
	if _, ok := a.authenticate(w, r, true); !ok {
		return
	}
	q, err := a.store.Quiz(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "quiz not found")
		return
	}
	if err != nil {
		a.internal(w, err)
		return
	}
	if export {
		q.ID = ""
		w.Header().Set("Content-Disposition", `attachment; filename="quiz.json"`)
	}
	writeJSON(w, 200, q)
}
func (a *App) deleteQuiz(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(w, r, true); !ok {
		return
	}
	res, err := a.store.db.ExecContext(r.Context(), "DELETE FROM quizzes WHERE id=?", r.PathValue("id"))
	if err != nil {
		a.internal(w, err)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeError(w, 404, "quiz not found")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) createRoom(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(w, r, true); !ok {
		return
	}
	var b struct {
		QuizID string `json:"quizId"`
	}
	if !decode(w, r, &b) {
		return
	}
	q, err := a.store.Quiz(r.Context(), b.QuizID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "quiz not found")
		return
	}
	if err != nil {
		a.internal(w, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	code := randomID()[:8]
	for {
		if _, ok := a.rooms[code]; !ok {
			break
		}
		code = randomID()[:8]
	}
	room := Room{Code: code, Quiz: q, Phase: "lobby", Revision: 1, Sequence: 1, CreatedAt: time.Now().UnixMilli(), Players: []Player{}, Answers: []Answer{}}
	if err = a.store.SaveRoom(r.Context(), room, nil); err != nil {
		a.internal(w, err)
		return
	}
	a.rooms[code] = room
	writeJSON(w, 200, map[string]string{"room": code})
}
func (a *App) join(w http.ResponseWriter, r *http.Request) {
	if a.limited(r, "join", 60) {
		writeError(w, 429, "too many attempts; retry in a minute")
		return
	}
	var b struct {
		Room string `json:"room"`
		Name string `json:"name"`
	}
	if !decode(w, r, &b) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	room, ok := a.rooms[strings.ToUpper(strings.TrimSpace(b.Room))]
	if !ok {
		writeError(w, 404, "room not found")
		return
	}
	room = cloneRoom(room)
	room.CloseExpired(time.Now().UnixMilli())
	id := randomID()
	if err := room.Join(id, b.Name); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	room.Sequence++
	// Room and session creation are serialized. If session creation fails, no player is admitted.
	token, s, err := a.store.NewSession(r.Context(), "player", room.Code, id)
	if err != nil {
		a.internal(w, err)
		return
	}
	if err = a.store.SaveRoom(r.Context(), room, nil); err != nil {
		_, _ = a.store.db.ExecContext(context.Background(), "DELETE FROM sessions WHERE id=?", s.ID)
		a.internal(w, err)
		return
	}
	a.rooms[room.Code] = room
	a.publish(room)
	writeJSON(w, 200, map[string]any{"token": token, "room": room.Code, "session": s})
}
func (a *App) action(w http.ResponseWriter, r *http.Request) {
	s, ok := a.authenticate(w, r, false)
	if !ok {
		return
	}
	var b struct {
		Room     string `json:"room"`
		Action   string `json:"action"`
		Revision int64  `json:"revision"`
		Question *int   `json:"question"`
		Choice   *int   `json:"choice"`
	}
	if !decode(w, r, &b) {
		return
	}
	if s.Role != "host" && b.Room != s.Room {
		writeError(w, 403, "wrong room")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	room, ok := a.rooms[b.Room]
	if !ok {
		writeError(w, 404, "room not found")
		return
	}
	room = cloneRoom(room)
	now := time.Now().UnixMilli()
	if room.CloseExpired(now) {
		room.Sequence++
		if err := a.store.SaveRoom(r.Context(), room, nil); err != nil {
			a.internal(w, err)
			return
		}
		a.rooms[room.Code] = room
		a.publish(room)
	}
	var answer *Answer
	var err error
	if b.Action == "answer" {
		if s.Role != "player" {
			writeError(w, 403, "only players can answer")
			return
		}
		if b.Question == nil || b.Choice == nil {
			writeError(w, 400, "question and choice required")
			return
		}
		answer, err = room.Submit(s.PlayerID, *b.Question, *b.Choice, b.Revision, now)
	} else {
		if s.Role != "host" {
			writeError(w, 403, "host access required")
			return
		}
		err = room.HostAction(b.Action, b.Revision, now)
	}
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	room.Sequence++
	if err = a.store.SaveRoom(r.Context(), room, answer); err != nil {
		a.internal(w, err)
		return
	}
	a.rooms[room.Code] = room
	a.publish(room)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) games(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(w, r, true); !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []map[string]any{}
	for _, room := range a.rooms {
		out = append(out, map[string]any{"code": room.Code, "title": room.Quiz.Title, "phase": room.Phase, "createdAt": room.CreatedAt, "playerCount": len(room.Players)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["createdAt"].(int64) > out[j]["createdAt"].(int64) })
	writeJSON(w, 200, out)
}
func (a *App) game(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(w, r, true); !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	room, ok := a.rooms[r.PathValue("id")]
	if !ok {
		writeError(w, 404, "game not found")
		return
	}
	// Raw answers and quiz keys are history-only; live clients always receive a snapshot.
	if room.Phase != "finished" {
		writeError(w, 409, "resume the active game to view its live state")
		return
	}
	writeJSON(w, 200, map[string]any{"game": room, "snapshot": room.Snapshot("", true, a.online(room.Code), time.Now().UnixMilli())})
}
func (a *App) online(code string) map[string]int {
	m := map[string]int{}
	for st := range a.streams {
		if st.room == code && st.session.Role == "player" {
			m[st.session.PlayerID]++
		}
	}
	return m
}
func (a *App) publish(room Room) {
	online := a.online(room.Code)
	now := time.Now().UnixMilli()
	for st := range a.streams {
		if st.room != room.Code {
			continue
		}
		b, _ := json.Marshal(map[string]any{"kind": "state", "snapshot": room.Snapshot(st.session.PlayerID, st.session.Role == "host", online, now)})
		select {
		case st.updates <- b:
		default:
			st.conn.CloseNow()
		}
	}
}
func (a *App) presence(code string) {
	r := a.rooms[code]
	r.Sequence++
	if err := a.store.SaveRoom(a.ctx, r, nil); err != nil {
		log.Printf("presence update failed: %v", err)
		return
	}
	a.rooms[code] = r
	a.publish(r)
}
func (a *App) live(w http.ResponseWriter, r *http.Request) {
	s, err := a.store.Session(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		writeError(w, 401, "session expired")
		return
	}
	code := strings.ToUpper(r.URL.Query().Get("room"))
	if s.Role == "player" {
		if code != "" && code != s.Room {
			writeError(w, 403, "wrong room")
			return
		}
		code = s.Room
	}
	a.mu.Lock()
	room, ok := a.rooms[code]
	if !ok || (s.Role == "player" && room.Player(s.PlayerID) == nil) {
		a.mu.Unlock()
		writeError(w, 404, "room not found")
		return
	}
	// Tunnel origins may differ from the upstream Host. The bearer session
	// above authenticates every connection before the WebSocket is accepted.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		a.mu.Unlock()
		return
	}
	st := &stream{session: s, room: code, conn: conn, updates: make(chan []byte, 16)}
	a.streams[st] = true
	a.presence(code)
	// A state must be sent even if a presence persistence error occurred.
	if len(st.updates) == 0 {
		b, _ := json.Marshal(map[string]any{"kind": "state", "snapshot": room.Snapshot(s.PlayerID, s.Role == "host", a.online(code), time.Now().UnixMilli())})
		st.updates <- b
	}
	a.mu.Unlock()
	defer func() {
		conn.CloseNow()
		a.mu.Lock()
		delete(a.streams, st)
		if a.ctx.Err() == nil {
			a.presence(code)
		}
		a.mu.Unlock()
	}()
	conn.SetReadLimit(4096)
	readCtx := conn.CloseRead(a.ctx)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-readCtx.Done():
			return
		case b := <-st.updates:
			if time.Now().UnixMilli() >= s.Expires {
				return
			}
			writeCtx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			if _, err = a.store.Session(a.ctx, r.URL.Query().Get("token")); err != nil {
				return
			}
			pingCtx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
			err = conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
			writeCtx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, []byte(`{"kind":"heartbeat"}`))
			cancel()
			if err != nil {
				return
			}
		}
	}
}
func (a *App) timers() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			for _, r := range a.rooms {
				if r.CloseExpired(time.Now().UnixMilli()) {
					r.Sequence++
					if err := a.store.SaveRoom(a.ctx, r, nil); err != nil {
						log.Printf("question timeout save failed: %v", err)
						continue
					}
					a.rooms[r.Code] = r
					a.publish(r)
				}
			}
			a.mu.Unlock()
		}
	}
}
func (a *App) closeStreams() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for st := range a.streams {
		st.conn.CloseNow()
	}
}
