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
	"fmt"
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
	if version < 0 || version > schemaVersion || version == 1 {
		return nil, fmt.Errorf("unsupported database schema version %d (expected %d)", version, schemaVersion)
	}
	if err = migrate(db, version); err != nil {
		return nil, err
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

const schemaVersion = 3

// migrations[i] upgrades the schema from version i+1 to i+2. A new database
// starts at version 0 and runs the first migration, which creates version 2.
var migrations = []func(tx *sql.Tx) error{
	migrateToV2,
	migrateToV3,
}

func migrate(db *sql.DB, version int) error {
	if version == 0 {
		version = 1
	}
	for ; version < schemaVersion; version++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err = migrations[version-1](tx); err == nil {
			_, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", version+1))
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil {
			return fmt.Errorf("migrate to schema version %d: %w", version+1, err)
		}
	}
	return nil
}

func migrateToV2(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE quizzes (id TEXT PRIMARY KEY, owner TEXT NOT NULL, document BLOB NOT NULL);
CREATE INDEX quizzes_owner ON quizzes(owner);
CREATE TABLE rooms (code TEXT PRIMARY KEY, owner TEXT NOT NULL, document BLOB NOT NULL);
CREATE TABLE answers (room_code TEXT NOT NULL REFERENCES rooms(code), player_id TEXT NOT NULL, question INTEGER NOT NULL, document BLOB NOT NULL, PRIMARY KEY(room_code,player_id,question));
CREATE TABLE sessions (id TEXT PRIMARY KEY, role TEXT NOT NULL, room_code TEXT NOT NULL, player_id TEXT NOT NULL, username TEXT NOT NULL, expires INTEGER NOT NULL);`)
	return err
}

// migrateToV3 replaces the quiz JSON documents with relational tables.
func migrateToV3(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT id,owner,document FROM quizzes ORDER BY rowid")
	if err != nil {
		return err
	}
	var quizzes []Quiz
	for rows.Next() {
		var document []byte
		var q Quiz
		if err = rows.Scan(&q.ID, &q.Owner, &document); err == nil {
			err = json.Unmarshal(document, &q)
		}
		if err != nil {
			rows.Close()
			return err
		}
		quizzes = append(quizzes, q)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	_, err = tx.Exec(`
DROP TABLE quizzes;
CREATE TABLE quizzes (id TEXT PRIMARY KEY, owner TEXT NOT NULL, version INTEGER NOT NULL, title TEXT NOT NULL);
CREATE INDEX quizzes_owner ON quizzes(owner);
CREATE TABLE questions (
	quiz_id TEXT NOT NULL REFERENCES quizzes(id) ON DELETE CASCADE,
	position INTEGER NOT NULL,
	text TEXT NOT NULL,
	correct_index INTEGER NOT NULL,
	time_limit_seconds INTEGER NOT NULL,
	PRIMARY KEY(quiz_id, position)
);
CREATE TABLE choices (
	quiz_id TEXT NOT NULL,
	question_position INTEGER NOT NULL,
	position INTEGER NOT NULL,
	text TEXT NOT NULL,
	PRIMARY KEY(quiz_id, question_position, position),
	FOREIGN KEY(quiz_id, question_position) REFERENCES questions(quiz_id, position) ON DELETE CASCADE
);`)
	if err != nil {
		return err
	}
	for _, q := range quizzes {
		if _, err = tx.Exec("INSERT INTO quizzes(id,owner,version,title) VALUES(?,?,?,?)", q.ID, q.Owner, q.Version, q.Title); err != nil {
			return err
		}
		if err = insertQuestions(context.Background(), tx, q); err != nil {
			return err
		}
	}
	return nil
}

func insertQuestions(ctx context.Context, tx *sql.Tx, q Quiz) error {
	for i, p := range q.Questions {
		if _, err := tx.ExecContext(ctx, "INSERT INTO questions(quiz_id,position,text,correct_index,time_limit_seconds) VALUES(?,?,?,?,?)", q.ID, i, p.Text, p.CorrectIndex, p.TimeLimitSeconds); err != nil {
			return err
		}
		for j, c := range p.Choices {
			if _, err := tx.ExecContext(ctx, "INSERT INTO choices(quiz_id,question_position,position,text) VALUES(?,?,?,?)", q.ID, i, j, c); err != nil {
				return err
			}
		}
	}
	return nil
}

func randomID() string { return rand.Text() }

func (repo *Repository) Close() error {
	return repo.db.Close()
}

// SaveQuiz also commits title changes to existing games in the same transaction.
// Their question copies and recorded answers remain unchanged.
func (repo *Repository) SaveQuiz(ctx context.Context, q Quiz, renamedRooms []Room) error {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT INTO quizzes(id,owner,version,title) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET version=excluded.version, title=excluded.title WHERE quizzes.owner=excluded.owner", q.ID, q.Owner, q.Version, q.Title)
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
	if _, err = tx.ExecContext(ctx, "DELETE FROM questions WHERE quiz_id=?", q.ID); err != nil {
		return err
	}
	if err = insertQuestions(ctx, tx, q); err != nil {
		return err
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
	return repo.loadQuizzes(ctx, "1=1")
}

func (repo *Repository) Quizzes(ctx context.Context, owner string) ([]Quiz, error) {
	return repo.loadQuizzes(ctx, "owner=?", owner)
}

func (repo *Repository) Quiz(ctx context.Context, id, owner string) (Quiz, error) {
	quizzes, err := repo.loadQuizzes(ctx, "id=? AND owner=?", id, owner)
	if err != nil {
		return Quiz{Owner: owner}, err
	}
	if len(quizzes) == 0 {
		return Quiz{Owner: owner}, ErrQuizNotFound
	}
	return quizzes[0], nil
}

// loadQuizzes returns the quizzes matching a condition on the quizzes table,
// newest first, with their questions and choices.
func (repo *Repository) loadQuizzes(ctx context.Context, where string, args ...any) ([]Quiz, error) {
	out := []Quiz{}
	byID := map[string]int{}
	rows, err := repo.db.QueryContext(ctx, "SELECT id,owner,version,title FROM quizzes WHERE "+where+" ORDER BY rowid DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var q Quiz
		if err = rows.Scan(&q.ID, &q.Owner, &q.Version, &q.Title); err != nil {
			return nil, err
		}
		q.Questions = []Question{}
		byID[q.ID] = len(out)
		out = append(out, q)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}
	subquery := "SELECT id FROM quizzes WHERE " + where
	questions, err := repo.db.QueryContext(ctx, "SELECT quiz_id,text,correct_index,time_limit_seconds FROM questions WHERE quiz_id IN ("+subquery+") ORDER BY quiz_id,position", args...)
	if err != nil {
		return nil, err
	}
	defer questions.Close()
	for questions.Next() {
		var id string
		p := Question{Choices: []string{}}
		if err = questions.Scan(&id, &p.Text, &p.CorrectIndex, &p.TimeLimitSeconds); err != nil {
			return nil, err
		}
		q := &out[byID[id]]
		q.Questions = append(q.Questions, p)
	}
	if err = questions.Err(); err != nil {
		return nil, err
	}
	questions.Close()
	choices, err := repo.db.QueryContext(ctx, "SELECT quiz_id,question_position,text FROM choices WHERE quiz_id IN ("+subquery+") ORDER BY quiz_id,question_position,position", args...)
	if err != nil {
		return nil, err
	}
	defer choices.Close()
	for choices.Next() {
		var id, text string
		var position int
		if err = choices.Scan(&id, &position, &text); err != nil {
			return nil, err
		}
		p := &out[byID[id]].Questions[position]
		p.Choices = append(p.Choices, text)
	}
	return out, choices.Err()
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
