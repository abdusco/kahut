package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Repository struct {
	db  *sql.DB
	key []byte
}

var (
	ErrQuizNotFound    = errors.New("quiz not found")
	ErrSessionNotFound = errors.New("session not found")
	ErrInvalidSession  = errors.New("invalid session")
	ErrSessionExpired  = errors.New("session expired")
)

func openRepository(path, signingKey string) (*Repository, error) {
	if len(signingKey) < 32 {
		return nil, errors.New("SESSION_SIGNING_KEY must contain at least 32 bytes")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	for _, stmt := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err = db.Exec(stmt); err != nil {
			return nil, err
		}
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version != 0 && version != 2 {
		return nil, errors.New("unsupported database schema; set DB_PATH to a fresh database for multi-user support")
	}
	if version == 0 {
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		_, err = tx.Exec(`
CREATE TABLE quizzes (id TEXT PRIMARY KEY, owner TEXT NOT NULL, document BLOB NOT NULL);
CREATE INDEX quizzes_owner ON quizzes(owner);
CREATE TABLE rooms (code TEXT PRIMARY KEY, owner TEXT NOT NULL, document BLOB NOT NULL);
CREATE TABLE answers (room_code TEXT NOT NULL REFERENCES rooms(code), player_id TEXT NOT NULL, question INTEGER NOT NULL, document BLOB NOT NULL, PRIMARY KEY(room_code,player_id,question));
CREATE TABLE sessions (id TEXT PRIMARY KEY, role TEXT NOT NULL, room_code TEXT NOT NULL, player_id TEXT NOT NULL, username TEXT NOT NULL, expires INTEGER NOT NULL);
PRAGMA user_version=2;`)
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	repo := &Repository{db: db, key: []byte(signingKey)}
	if path != ":memory:" {
		if err = os.Chmod(path, 0600); err != nil {
			return nil, err
		}
	}
	ok = true
	return repo, nil
}

func randomID() string { return rand.Text() }

func (repo *Repository) Close() error {
	return repo.db.Close()
}

// SaveQuiz also commits title changes to existing games in the same transaction.
// Their question copies and recorded answers remain unchanged.
func (repo *Repository) SaveQuiz(ctx context.Context, q Quiz, renamedRooms []Room) error {
	b, err := json.Marshal(q)
	if err != nil {
		return err
	}
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT INTO quizzes(id,owner,document) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET document=excluded.document WHERE quizzes.owner=excluded.owner", q.ID, q.Owner, b)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrQuizNotFound
	}
	for _, room := range renamedRooms {
		document, err := json.Marshal(room)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE rooms SET document=? WHERE code=?", document, room.Code); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AllQuizzes is used when restoring room titles at server startup.
func (repo *Repository) AllQuizzes(ctx context.Context) ([]Quiz, error) {
	rows, err := repo.db.QueryContext(ctx, "SELECT owner,document FROM quizzes ORDER BY rowid DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Quiz{}
	for rows.Next() {
		var b []byte
		var q Quiz
		if err = rows.Scan(&q.Owner, &b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &q); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (repo *Repository) Quizzes(ctx context.Context, owner string) ([]Quiz, error) {
	rows, err := repo.db.QueryContext(ctx, "SELECT document FROM quizzes WHERE owner=? ORDER BY rowid DESC", owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Quiz{}
	for rows.Next() {
		var document []byte
		quiz := Quiz{Owner: owner}
		if err = rows.Scan(&document); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(document, &quiz); err != nil {
			return nil, err
		}
		out = append(out, quiz)
	}
	return out, rows.Err()
}

func (repo *Repository) Quiz(ctx context.Context, id, owner string) (Quiz, error) {
	var b []byte
	q := Quiz{Owner: owner}
	err := repo.db.QueryRowContext(ctx, "SELECT document FROM quizzes WHERE id=? AND owner=?", id, owner).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return q, ErrQuizNotFound
	}
	if err != nil {
		return q, err
	}
	err = json.Unmarshal(b, &q)
	return q, err
}

func (repo *Repository) DeleteQuiz(ctx context.Context, id, owner string) error {
	result, err := repo.db.ExecContext(ctx, "DELETE FROM quizzes WHERE id=? AND owner=?", id, owner)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrQuizNotFound
	}
	return nil
}

func (repo *Repository) SaveRoom(ctx context.Context, r Room, a *Answer) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "INSERT INTO rooms(code,owner,document) VALUES(?,?,?) ON CONFLICT(code) DO UPDATE SET document=excluded.document", r.Code, r.Owner, b)
	if err != nil {
		return err
	}
	if a != nil {
		ab, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO answers(room_code,player_id,question,document) VALUES(?,?,?,?)", r.Code, a.PlayerID, a.Question, ab); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (repo *Repository) Rooms(ctx context.Context) ([]Room, error) {
	rows, err := repo.db.QueryContext(ctx, "SELECT owner,document FROM rooms ORDER BY rowid DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		var b []byte
		var r Room
		if err = rows.Scan(&r.Owner, &b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Session struct {
	Username string `json:"username,omitempty"`
	ID       string `json:"-"`
	Role     string `json:"role"`
	Room     string `json:"room"`
	PlayerID string `json:"playerId"`
	Expires  int64  `json:"expires"`
}

func (repo *Repository) sign(id string) string {
	m := hmac.New(sha256.New, repo.key)
	m.Write([]byte(id))
	return id + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (repo *Repository) NewSession(ctx context.Context, role, room, player, username string) (string, Session, error) {
	duration := 30 * 24 * time.Hour
	if role == "host" {
		duration = 12 * time.Hour
	}
	v := Session{ID: randomID(), Role: role, Room: room, PlayerID: player, Username: username, Expires: time.Now().Add(duration).UnixMilli()}
	_, err := repo.db.ExecContext(ctx, "INSERT INTO sessions(id,role,room_code,player_id,username,expires) VALUES(?,?,?,?,?,?)", v.ID, v.Role, v.Room, v.PlayerID, v.Username, v.Expires)
	return repo.sign(v.ID), v, err
}

func (repo *Repository) Session(ctx context.Context, token string) (Session, error) {
	var v Session
	id, _, ok := strings.Cut(token, ".")
	if !ok || len(token) > 200 || !hmac.Equal([]byte(repo.sign(id)), []byte(token)) {
		return v, ErrInvalidSession
	}
	err := repo.db.QueryRowContext(ctx, "SELECT id,role,room_code,player_id,username,expires FROM sessions WHERE id=?", id).Scan(&v.ID, &v.Role, &v.Room, &v.PlayerID, &v.Username, &v.Expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return v, err
	}
	if time.Now().UnixMilli() >= v.Expires {
		return Session{}, ErrSessionExpired
	}
	return v, nil
}

func (repo *Repository) DeleteSession(ctx context.Context, id string) error {
	result, err := repo.db.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrSessionNotFound
	}
	return nil
}
