package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"
	"github.com/samber/lo"
)

var pageRoute = regexp.MustCompile(`^/(host(/quizzes/(new|[A-Za-z0-9_-]+/edit)|/games/[A-Za-z0-9]{8})?|rooms/[A-Za-z0-9]{8}|resume)?$`)

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
	ctx     context.Context
	repo    *Repository
	users   map[string][32]byte
	mu      sync.Mutex
	rooms   map[string]Room
	streams map[*stream]bool
	rates   map[string]rateWindow
}

func newApp(ctx context.Context, s *Repository, authUsers string) (*App, error) {
	users, err := parseAuthUsers(authUsers)
	if err != nil {
		return nil, err
	}
	a := &App{ctx: ctx, repo: s, users: users, rooms: map[string]Room{}, streams: map[*stream]bool{}, rates: map[string]rateWindow{}}
	rooms, err := s.Rooms(ctx)
	if err != nil {
		return nil, err
	}
	quizzes, err := s.AllQuizzes(ctx)
	if err != nil {
		return nil, err
	}
	titles := make(map[[2]string]string, len(quizzes))
	for _, quiz := range quizzes {
		titles[[2]string{quiz.Owner, quiz.ID}] = quiz.Title
	}
	for _, r := range rooms {
		changed := r.CloseExpired(time.Now().UnixMilli())
		// Older databases retain the original title in each game copy.
		if title, exists := titles[[2]string{r.Owner, r.Quiz.ID}]; exists && title != r.Quiz.Title {
			r.Quiz.Title = title
			changed = true
		}
		if changed {
			r.Sequence++
			if err = s.SaveRoom(ctx, r, nil); err != nil {
				return nil, err
			}
		}
		a.rooms[r.Code] = r
	}
	return a, nil
}

func cloneRoom(r Room) Room {
	b, _ := json.Marshal(r)
	var c Room
	_ = json.Unmarshal(b, &c)
	c.Owner = r.Owner
	c.Quiz.Owner = r.Quiz.Owner
	return c
}

func (a *App) routes() http.Handler {
	e := echo.New()
	e.JSONSerializer = strictJSONSerializer{}
	e.HTTPErrorHandler = func(c *echo.Context, err error) {
		response, unwrapErr := echo.UnwrapResponse(c.Response())
		if unwrapErr != nil || response.Committed {
			return
		}
		status, message := http.StatusInternalServerError, "storage operation failed; please retry"
		var httpError *echo.HTTPError
		var statusError echo.HTTPStatusCoder
		if errors.Is(err, ErrQuizNotFound) {
			status, message = http.StatusNotFound, ErrQuizNotFound.Error()
		} else if errors.Is(err, ErrInvalidSession) || errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrSessionExpired) {
			status, message = http.StatusUnauthorized, "session expired; sign in or join again"
		} else if errors.As(err, &httpError) {
			status, message = httpError.Code, httpError.Message
			if message == "" {
				message = http.StatusText(status)
			}
		} else if errors.As(err, &statusError) {
			status = statusError.StatusCode()
			message = http.StatusText(status)
		} else {
			log.Printf("request failed: %v", err)
		}
		if err := c.JSON(status, map[string]string{"error": message}); err != nil {
			log.Printf("error response failed: %v", err)
		}
	}
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			h := c.Response().Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("X-Frame-Options", "DENY")
			if strings.HasPrefix(c.Request().URL.Path, "/api/") {
				h.Set("Cache-Control", "no-store")
				h.Set("Access-Control-Allow-Origin", "*")
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				if c.Request().Method == http.MethodOptions {
					return c.NoContent(http.StatusNoContent)
				}
			}
			return next(c)
		}
	})
	host := a.requireSession(true)
	session := a.requireSession(false)
	e.POST("/api/host/login", a.login)
	e.POST("/api/host/logout", a.logout, host)
	e.GET("/api/session", a.session, session)
	e.GET("/api/live", a.live, session)
	e.GET("/api/quizzes", a.quizzes, host)
	e.POST("/api/quizzes", a.saveQuiz, host)
	e.POST("/api/quizzes/import", a.saveQuiz, host)
	e.GET("/api/quizzes/:id", a.getQuiz, host)
	e.PUT("/api/quizzes/:id", a.saveQuiz, host)
	e.DELETE("/api/quizzes/:id", a.deleteQuiz, host)
	e.GET("/api/quizzes/:id/export", a.exportQuiz, host)
	e.POST("/api/rooms", a.createRoom, host)
	e.POST("/api/join", a.join)
	e.POST("/api/action", a.action, session)
	e.GET("/api/games", a.games, host)
	e.GET("/api/games/:id", a.game, host)
	var files fs.FS
	if os.Getenv("DEBUG") == "1" {
		files = os.DirFS("assets")
	} else {
		files, _ = fs.Sub(assets, "assets")
	}
	servePage := func(c *echo.Context) error {
		r := c.Request()
		if strings.HasPrefix(r.URL.Path, "/api/") {
			return echo.ErrNotFound
		}
		canonical := strings.TrimSuffix(r.URL.Path, "/")
		if r.URL.Path != "/" && canonical != r.URL.Path && pageRoute.MatchString(canonical) {
			target := *r.URL
			target.Path = canonical
			return c.Redirect(http.StatusPermanentRedirect, target.String())
		}
		if pageRoute.MatchString(r.URL.Path) {
			c.Response().Header().Set("Cache-Control", "no-cache")
			return c.FileFS("index.html", files)
		}
		if r.URL.Path == "/" || r.URL.Path == "/sw.js" {
			c.Response().Header().Set("Cache-Control", "no-cache")
		}
		return c.FileFS(strings.TrimPrefix(r.URL.Path, "/"), files)
	}
	e.Any("/", servePage)
	e.Any("/*", servePage)
	return e
}

// Echo's binder uses this serializer for JSON request bodies. Keep quiz imports
// strict while using Echo's standard response serialization.
type strictJSONSerializer struct {
	echo.DefaultJSONSerializer
}

func (strictJSONSerializer) Deserialize(c *echo.Context, target any) error {
	r := c.Request()
	r.Body = http.MaxBytesReader(c.Response(), r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return echo.NewHTTPError(http.StatusBadRequest, "expected one JSON document")
	}
	return nil
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
func (a *App) login(c *echo.Context) error {
	r := c.Request()
	if a.limited(r, "login", 10) {
		return echo.NewHTTPError(http.StatusTooManyRequests, "too many attempts; retry in a minute")
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := echo.BindBody(c, &b); err != nil {
		return err
	}
	x := sha256.Sum256([]byte(b.Password))
	y, exists := a.users[b.Username]
	if subtle.ConstantTimeCompare(x[:], y[:]) != 1 || !exists {
		return echo.NewHTTPError(http.StatusUnauthorized, "incorrect username or password")
	}
	token, s, err := a.repo.NewSession(r.Context(), "host", "", "", b.Username)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"token": token, "session": s})
}
func (a *App) logout(c *echo.Context) error {
	r := c.Request()
	s, _ := echo.ContextGet[Session](c, "session")
	if err := a.repo.DeleteSession(r.Context(), s.ID); err != nil {
		return err
	}
	a.mu.Lock()
	for st := range a.streams {
		if st.session.ID == s.ID {
			st.conn.CloseNow()
		}
	}
	a.mu.Unlock()
	return c.JSON(http.StatusOK, map[string]bool{"ok": true})
}
func (a *App) session(c *echo.Context) error {
	session, _ := echo.ContextGet[Session](c, "session")
	return c.JSON(http.StatusOK, session)
}
func (a *App) quizzes(c *echo.Context) error {
	r := c.Request()
	s, _ := echo.ContextGet[Session](c, "session")
	q, err := a.repo.Quizzes(r.Context(), s.Username)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, q)
}
func (a *App) saveQuiz(c *echo.Context) error {
	r := c.Request()
	s, _ := echo.ContextGet[Session](c, "session")
	var q Quiz
	if err := echo.BindBody(c, &q); err != nil {
		return err
	}
	q.Owner = s.Username
	if err := q.Validate(); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if id := c.Param("id"); id != "" {
		if _, err := a.repo.Quiz(r.Context(), id, s.Username); err != nil {
			return err
		}
		q.ID = id
	} else {
		q.ID = randomID()
	}
	var renamedRooms []Room
	for _, room := range a.rooms {
		if room.Owner == s.Username && room.Quiz.ID == q.ID && room.Quiz.Title != q.Title {
			room = cloneRoom(room)
			room.Quiz.Title = q.Title
			room.Sequence++
			renamedRooms = append(renamedRooms, room)
		}
	}
	if err := a.repo.SaveQuiz(r.Context(), q, renamedRooms); err != nil {
		return err
	}
	for _, room := range renamedRooms {
		a.rooms[room.Code] = room
		a.publish(room)
	}
	return c.JSON(http.StatusOK, q)
}
func (a *App) getQuiz(c *echo.Context) error {
	return a.readQuiz(c, false)
}

func (a *App) exportQuiz(c *echo.Context) error {
	return a.readQuiz(c, true)
}

func (a *App) readQuiz(c *echo.Context, export bool) error {
	r := c.Request()
	s, _ := echo.ContextGet[Session](c, "session")
	q, err := a.repo.Quiz(r.Context(), c.Param("id"), s.Username)
	if err != nil {
		return err
	}
	if export {
		q.ID = ""
		c.Response().Header().Set("Content-Disposition", `attachment; filename="quiz.json"`)
	}
	return c.JSON(http.StatusOK, q)
}
func (a *App) deleteQuiz(c *echo.Context) error {
	r := c.Request()
	s, _ := echo.ContextGet[Session](c, "session")
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.repo.DeleteQuiz(r.Context(), c.Param("id"), s.Username); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]bool{"ok": true})
}
func (a *App) createRoom(c *echo.Context) error {
	r := c.Request()
	s, _ := echo.ContextGet[Session](c, "session")
	var b struct {
		QuizID string `json:"quizId"`
	}
	if err := echo.BindBody(c, &b); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	q, err := a.repo.Quiz(r.Context(), b.QuizID, s.Username)
	if err != nil {
		return err
	}
	code := randomID()[:8]
	for {
		if _, ok := a.rooms[code]; !ok {
			break
		}
		code = randomID()[:8]
	}
	room := Room{Owner: s.Username, Code: code, Quiz: q, Phase: "lobby", Revision: 1, Sequence: 1, CreatedAt: time.Now().UnixMilli(), Players: []Player{}, Answers: []Answer{}}
	if err = a.repo.SaveRoom(r.Context(), room, nil); err != nil {
		return err
	}
	a.rooms[code] = room
	return c.JSON(http.StatusOK, map[string]string{"room": code})
}
func (a *App) join(c *echo.Context) error {
	r := c.Request()
	if a.limited(r, "join", 60) {
		return echo.NewHTTPError(http.StatusTooManyRequests, "too many attempts; retry in a minute")
	}
	var b struct {
		Room string `json:"room"`
		Name string `json:"name"`
	}
	if err := echo.BindBody(c, &b); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	room, ok := a.rooms[strings.ToUpper(strings.TrimSpace(b.Room))]
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "room not found")
	}
	room = cloneRoom(room)
	room.CloseExpired(time.Now().UnixMilli())
	id := randomID()
	if err := room.Join(id, b.Name); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	room.Sequence++
	// Room and session creation are serialized. If session creation fails, no player is admitted.
	token, s, err := a.repo.NewSession(r.Context(), "player", room.Code, id, "")
	if err != nil {
		return err
	}
	if err = a.repo.SaveRoom(r.Context(), room, nil); err != nil {
		_ = a.repo.DeleteSession(context.Background(), s.ID)
		return err
	}
	a.rooms[room.Code] = room
	a.publish(room)
	return c.JSON(http.StatusOK, map[string]any{"token": token, "room": room.Code, "session": s})
}
func (a *App) action(c *echo.Context) error {
	r := c.Request()
	s, _ := echo.ContextGet[Session](c, "session")
	var b struct {
		Room     string `json:"room"`
		Action   string `json:"action"`
		Revision int64  `json:"revision"`
		Question *int   `json:"question"`
		Choice   *int   `json:"choice"`
	}
	if err := echo.BindBody(c, &b); err != nil {
		return err
	}
	if s.Role != "host" && b.Room != s.Room {
		return echo.NewHTTPError(http.StatusForbidden, "wrong room")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	room, ok := a.rooms[b.Room]
	if !ok || (s.Role == "host" && room.Owner != s.Username) {
		return echo.NewHTTPError(http.StatusNotFound, "room not found")
	}
	room = cloneRoom(room)
	now := time.Now().UnixMilli()
	if room.CloseExpired(now) {
		room.Sequence++
		if err := a.repo.SaveRoom(r.Context(), room, nil); err != nil {
			return err
		}
		a.rooms[room.Code] = room
		a.publish(room)
	}
	var answer *Answer
	var err error
	if b.Action == "answer" {
		if s.Role != "player" {
			return echo.NewHTTPError(http.StatusForbidden, "only players can answer")
		}
		if b.Question == nil || b.Choice == nil {
			return echo.NewHTTPError(http.StatusBadRequest, "question and choice required")
		}
		answer, err = room.Submit(s.PlayerID, *b.Question, *b.Choice, b.Revision, now)
	} else {
		if s.Role != "host" {
			return echo.NewHTTPError(http.StatusForbidden, "host access required")
		}
		err = room.HostAction(b.Action, b.Revision, now)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	room.Sequence++
	if err = a.repo.SaveRoom(r.Context(), room, answer); err != nil {
		return err
	}
	a.rooms[room.Code] = room
	a.publish(room)
	return c.JSON(http.StatusOK, map[string]bool{"ok": true})
}
func (a *App) games(c *echo.Context) error {
	s, _ := echo.ContextGet[Session](c, "session")
	a.mu.Lock()
	defer a.mu.Unlock()
	out := lo.FilterMap(lo.Values(a.rooms), func(room Room, _ int) (map[string]any, bool) {
		if room.Owner != s.Username {
			return nil, false
		}
		return map[string]any{"code": room.Code, "title": room.Quiz.Title, "phase": room.Phase, "createdAt": room.CreatedAt, "playerCount": len(room.Players)}, true
	})
	sort.Slice(out, func(i, j int) bool { return out[i]["createdAt"].(int64) > out[j]["createdAt"].(int64) })
	return c.JSON(http.StatusOK, out)
}
func (a *App) game(c *echo.Context) error {
	s, _ := echo.ContextGet[Session](c, "session")
	a.mu.Lock()
	defer a.mu.Unlock()
	room, ok := a.rooms[c.Param("id")]
	if !ok || room.Owner != s.Username {
		return echo.NewHTTPError(http.StatusNotFound, "game not found")
	}
	// Raw answers and quiz keys are history-only; live clients always receive a snapshot.
	if room.Phase != "finished" {
		return echo.NewHTTPError(http.StatusConflict, "resume the active game to view its live state")
	}
	return c.JSON(http.StatusOK, map[string]any{"game": room, "snapshot": room.Snapshot("", true, a.online(room.Code), time.Now().UnixMilli())})
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
	if err := a.repo.SaveRoom(a.ctx, r, nil); err != nil {
		log.Printf("presence update failed: %v", err)
		return
	}
	a.rooms[code] = r
	a.publish(r)
}
func (a *App) live(c *echo.Context) error {
	r := c.Request()
	s, _ := echo.ContextGet[Session](c, "session")
	code := strings.ToUpper(r.URL.Query().Get("room"))
	if s.Role == "player" {
		if code != "" && code != s.Room {
			return echo.NewHTTPError(http.StatusForbidden, "wrong room")
		}
		code = s.Room
	}
	a.mu.Lock()
	room, ok := a.rooms[code]
	if !ok || (s.Role == "host" && room.Owner != s.Username) || (s.Role == "player" && room.Player(s.PlayerID) == nil) {
		a.mu.Unlock()
		return echo.NewHTTPError(http.StatusNotFound, "room not found")
	}
	// Tunnel origins may differ from the upstream Host. The bearer session
	// above authenticates every connection before the WebSocket is accepted.
	conn, err := websocket.Accept(c.Response(), r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		a.mu.Unlock()
		return nil
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
			return nil
		case <-readCtx.Done():
			return nil
		case b := <-st.updates:
			if time.Now().UnixMilli() >= s.Expires {
				return nil
			}
			writeCtx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return nil
			}
		case <-ticker.C:
			if _, err = a.repo.Session(a.ctx, r.URL.Query().Get("token")); err != nil {
				return nil
			}
			pingCtx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
			err = conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return nil
			}
			writeCtx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, []byte(`{"kind":"heartbeat"}`))
			cancel()
			if err != nil {
				return nil
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
					if err := a.repo.SaveRoom(a.ctx, r, nil); err != nil {
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
