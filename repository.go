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
	"log"
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
	ErrImageNotFound   = errors.New("image not found")
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

const schemaVersion = 5

// migrations[i] upgrades the schema from version i+1 to i+2. A new database
// starts at version 0 and runs the first migration, which creates version 2.
var migrations = []func(tx *sql.Tx) error{
	migrateToV2,
	migrateToV3,
	migrateToV4,
	migrateToV5,
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
		if err = insertQuestionsV3(context.Background(), tx, q); err != nil {
			return err
		}
	}
	return nil
}

// migrateToV5 adds image storage. Images are immutable and referenced by
// quiz questions and by the question copies kept in rooms.
func migrateToV5(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE images (
	id TEXT PRIMARY KEY,
	owner TEXT NOT NULL,
	content_type TEXT NOT NULL,
	data BLOB NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX images_owner ON images(owner);
ALTER TABLE questions ADD COLUMN image_id TEXT REFERENCES images(id);
ALTER TABLE room_questions ADD COLUMN image_id TEXT REFERENCES images(id);`)
	return err
}

// migrateToV4 replaces the room and answer JSON documents with relational
// tables. Rooms keep their own copy of the quiz questions so later quiz edits
// do not change running or finished games.
func migrateToV4(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT owner,document FROM rooms ORDER BY rowid")
	if err != nil {
		return err
	}
	var rooms []Room
	for rows.Next() {
		var document []byte
		var r Room
		if err = rows.Scan(&r.Owner, &document); err == nil {
			err = json.Unmarshal(document, &r)
		}
		if err != nil {
			rows.Close()
			return err
		}
		rooms = append(rooms, r)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	_, err = tx.Exec(`
DROP TABLE answers;
DROP TABLE rooms;
CREATE TABLE rooms (
	code TEXT PRIMARY KEY,
	owner TEXT NOT NULL,
	quiz_id TEXT NOT NULL,
	quiz_version INTEGER NOT NULL,
	quiz_title TEXT NOT NULL,
	phase TEXT NOT NULL,
	question INTEGER NOT NULL,
	revision INTEGER NOT NULL,
	sequence INTEGER NOT NULL,
	started_at INTEGER NOT NULL,
	deadline INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE room_questions (
	room_code TEXT NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
	position INTEGER NOT NULL,
	text TEXT NOT NULL,
	correct_index INTEGER NOT NULL,
	time_limit_seconds INTEGER NOT NULL,
	PRIMARY KEY(room_code, position)
);
CREATE TABLE room_choices (
	room_code TEXT NOT NULL,
	question_position INTEGER NOT NULL,
	position INTEGER NOT NULL,
	text TEXT NOT NULL,
	PRIMARY KEY(room_code, question_position, position),
	FOREIGN KEY(room_code, question_position) REFERENCES room_questions(room_code, position) ON DELETE CASCADE
);
CREATE TABLE players (
	room_code TEXT NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
	id TEXT NOT NULL,
	position INTEGER NOT NULL,
	name TEXT NOT NULL,
	score INTEGER NOT NULL,
	from_question INTEGER NOT NULL,
	PRIMARY KEY(room_code, id)
);
CREATE TABLE answers (
	room_code TEXT NOT NULL,
	player_id TEXT NOT NULL,
	question INTEGER NOT NULL,
	choice INTEGER NOT NULL,
	points INTEGER NOT NULL,
	received_at INTEGER NOT NULL,
	PRIMARY KEY(room_code, player_id, question),
	FOREIGN KEY(room_code, player_id) REFERENCES players(room_code, id) ON DELETE CASCADE
);`)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, r := range rooms {
		if err = insertRoomV4(ctx, tx, r); err != nil {
			return err
		}
		if err = upsertPlayers(ctx, tx, r); err != nil {
			return err
		}
		for _, a := range r.Answers {
			if err = insertAnswer(ctx, tx, r.Code, a); err != nil {
				return err
			}
		}
	}
	return nil
}

// insertRoomV4 is frozen at the schema used by migrateToV4.
func insertRoomV4(ctx context.Context, tx *sql.Tx, r Room) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO rooms(code,owner,quiz_id,quiz_version,quiz_title,phase,question,revision,sequence,started_at,deadline,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		r.Code, r.Owner, r.Quiz.ID, r.Quiz.Version, r.Quiz.Title, r.Phase, r.Question, r.Revision, r.Sequence, r.StartedAt, r.Deadline, r.CreatedAt); err != nil {
		return err
	}
	for i, p := range r.Quiz.Questions {
		if _, err := tx.ExecContext(ctx, "INSERT INTO room_questions(room_code,position,text,correct_index,time_limit_seconds) VALUES(?,?,?,?,?)", r.Code, i, p.Text, p.CorrectIndex, p.TimeLimitSeconds); err != nil {
			return err
		}
		for j, c := range p.Choices {
			if _, err := tx.ExecContext(ctx, "INSERT INTO room_choices(room_code,question_position,position,text) VALUES(?,?,?,?)", r.Code, i, j, c); err != nil {
				return err
			}
		}
	}
	return nil
}

func insertRoomQuestions(ctx context.Context, tx *sql.Tx, r Room) error {
	for i, p := range r.Quiz.Questions {
		if _, err := tx.ExecContext(ctx, "INSERT INTO room_questions(room_code,position,text,correct_index,time_limit_seconds,image_id) VALUES(?,?,?,?,?,?)", r.Code, i, p.Text, p.CorrectIndex, p.TimeLimitSeconds, nullable(p.ImageID)); err != nil {
			return err
		}
		for j, c := range p.Choices {
			if _, err := tx.ExecContext(ctx, "INSERT INTO room_choices(room_code,question_position,position,text) VALUES(?,?,?,?)", r.Code, i, j, c); err != nil {
				return err
			}
		}
	}
	return nil
}

func insertQuestions(ctx context.Context, tx *sql.Tx, q Quiz) error {
	for i, p := range q.Questions {
		if _, err := tx.ExecContext(ctx, "INSERT INTO questions(quiz_id,position,text,correct_index,time_limit_seconds,image_id) VALUES(?,?,?,?,?,?)", q.ID, i, p.Text, p.CorrectIndex, p.TimeLimitSeconds, nullable(p.ImageID)); err != nil {
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

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func upsertPlayers(ctx context.Context, tx *sql.Tx, r Room) error {
	for i, p := range r.Players {
		if _, err := tx.ExecContext(ctx, "INSERT INTO players(room_code,id,position,name,score,from_question) VALUES(?,?,?,?,?,?) ON CONFLICT(room_code,id) DO UPDATE SET name=excluded.name, score=excluded.score, from_question=excluded.from_question", r.Code, p.ID, i, p.Name, p.Score, p.FromQuestion); err != nil {
			return err
		}
	}
	return nil
}

func insertAnswer(ctx context.Context, tx *sql.Tx, room string, a Answer) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO answers(room_code,player_id,question,choice,points,received_at) VALUES(?,?,?,?,?,?)", room, a.PlayerID, a.Question, a.Choice, a.Points, a.ReceivedAt)
	return err
}

// insertQuestionsV3 is frozen at the schema used by migrateToV3.
func insertQuestionsV3(ctx context.Context, tx *sql.Tx, q Quiz) error {
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
	for _, p := range q.Questions {
		if p.ImageID == "" {
			continue
		}
		var found int
		err = tx.QueryRowContext(ctx, "SELECT 1 FROM images WHERE id=? AND owner=?", p.ImageID, q.Owner).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrImageNotFound
		}
		if err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM questions WHERE quiz_id=?", q.ID); err != nil {
		return err
	}
	if err = insertQuestions(ctx, tx, q); err != nil {
		return err
	}
	for _, room := range renamedRooms {
		if _, err = tx.ExecContext(ctx, "UPDATE rooms SET quiz_title=? WHERE code=?", room.Quiz.Title, room.Code); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	repo.pruneImages(ctx)
	return nil
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
	questions, err := repo.db.QueryContext(ctx, "SELECT quiz_id,text,correct_index,time_limit_seconds,COALESCE(image_id,'') FROM questions WHERE quiz_id IN ("+subquery+") ORDER BY quiz_id,position", args...)
	if err != nil {
		return nil, err
	}
	defer questions.Close()
	for questions.Next() {
		var id string
		p := Question{Choices: []string{}}
		if err = questions.Scan(&id, &p.Text, &p.CorrectIndex, &p.TimeLimitSeconds, &p.ImageID); err != nil {
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
	repo.pruneImages(ctx)
	return nil
}

// SaveRoom stores the room state, its players and the optional new answer.
// The quiz questions are copied once, when the room is first saved.
func (repo *Repository) SaveRoom(ctx context.Context, r Room, a *Answer) error {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT INTO rooms(code,owner,quiz_id,quiz_version,quiz_title,phase,question,revision,sequence,started_at,deadline,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(code) DO NOTHING",
		r.Code, r.Owner, r.Quiz.ID, r.Quiz.Version, r.Quiz.Title, r.Phase, r.Question, r.Revision, r.Sequence, r.StartedAt, r.Deadline, r.CreatedAt)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		_, err = tx.ExecContext(ctx, "UPDATE rooms SET quiz_title=?,phase=?,question=?,revision=?,sequence=?,started_at=?,deadline=? WHERE code=?",
			r.Quiz.Title, r.Phase, r.Question, r.Revision, r.Sequence, r.StartedAt, r.Deadline, r.Code)
		if err != nil {
			return err
		}
	} else if err = insertRoomQuestions(ctx, tx, r); err != nil {
		return err
	}
	if err = upsertPlayers(ctx, tx, r); err != nil {
		return err
	}
	if a != nil {
		if err = insertAnswer(ctx, tx, r.Code, *a); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (repo *Repository) Rooms(ctx context.Context) ([]Room, error) {
	out := []Room{}
	byCode := map[string]int{}
	rows, err := repo.db.QueryContext(ctx, "SELECT code,owner,quiz_id,quiz_version,quiz_title,phase,question,revision,sequence,started_at,deadline,created_at FROM rooms ORDER BY rowid DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r := Room{Players: []Player{}, Answers: []Answer{}}
		r.Quiz.Questions = []Question{}
		if err = rows.Scan(&r.Code, &r.Owner, &r.Quiz.ID, &r.Quiz.Version, &r.Quiz.Title, &r.Phase, &r.Question, &r.Revision, &r.Sequence, &r.StartedAt, &r.Deadline, &r.CreatedAt); err != nil {
			return nil, err
		}
		byCode[r.Code] = len(out)
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	questions, err := repo.db.QueryContext(ctx, "SELECT room_code,text,correct_index,time_limit_seconds,COALESCE(image_id,'') FROM room_questions ORDER BY room_code,position")
	if err != nil {
		return nil, err
	}
	defer questions.Close()
	for questions.Next() {
		var code string
		p := Question{Choices: []string{}}
		if err = questions.Scan(&code, &p.Text, &p.CorrectIndex, &p.TimeLimitSeconds, &p.ImageID); err != nil {
			return nil, err
		}
		r := &out[byCode[code]]
		r.Quiz.Questions = append(r.Quiz.Questions, p)
	}
	if err = questions.Err(); err != nil {
		return nil, err
	}
	questions.Close()

	choices, err := repo.db.QueryContext(ctx, "SELECT room_code,question_position,text FROM room_choices ORDER BY room_code,question_position,position")
	if err != nil {
		return nil, err
	}
	defer choices.Close()
	for choices.Next() {
		var code, text string
		var position int
		if err = choices.Scan(&code, &position, &text); err != nil {
			return nil, err
		}
		p := &out[byCode[code]].Quiz.Questions[position]
		p.Choices = append(p.Choices, text)
	}
	if err = choices.Err(); err != nil {
		return nil, err
	}
	choices.Close()

	players, err := repo.db.QueryContext(ctx, "SELECT room_code,id,name,score,from_question FROM players ORDER BY room_code,position")
	if err != nil {
		return nil, err
	}
	defer players.Close()
	for players.Next() {
		var code string
		var p Player
		if err = players.Scan(&code, &p.ID, &p.Name, &p.Score, &p.FromQuestion); err != nil {
			return nil, err
		}
		r := &out[byCode[code]]
		r.Players = append(r.Players, p)
	}
	if err = players.Err(); err != nil {
		return nil, err
	}
	players.Close()

	answers, err := repo.db.QueryContext(ctx, "SELECT room_code,player_id,question,choice,points,received_at FROM answers ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer answers.Close()
	for answers.Next() {
		var code string
		var a Answer
		if err = answers.Scan(&code, &a.PlayerID, &a.Question, &a.Choice, &a.Points, &a.ReceivedAt); err != nil {
			return nil, err
		}
		r := &out[byCode[code]]
		r.Answers = append(r.Answers, a)
	}
	return out, answers.Err()
}

// SaveImage stores an uploaded image and returns its unguessable ID.
func (repo *Repository) SaveImage(ctx context.Context, owner, contentType string, data []byte) (string, error) {
	id := randomID()
	_, err := repo.db.ExecContext(ctx, "INSERT INTO images(id,owner,content_type,data,created_at) VALUES(?,?,?,?,?)", id, owner, contentType, data, time.Now().UnixMilli())
	return id, err
}

func (repo *Repository) Image(ctx context.Context, id string) (contentType string, data []byte, err error) {
	err = repo.db.QueryRowContext(ctx, "SELECT content_type,data FROM images WHERE id=?", id).Scan(&contentType, &data)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrImageNotFound
	}
	return
}

// pruneImages removes images that no quiz or game uses. Recent uploads are kept
// because the editor uploads before the quiz is saved. Failures are only
// logged: a leftover image is harmless.
func (repo *Repository) pruneImages(ctx context.Context) {
	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
	_, err := repo.db.ExecContext(ctx, `DELETE FROM images WHERE created_at<? AND id NOT IN (
SELECT image_id FROM questions WHERE image_id IS NOT NULL
UNION SELECT image_id FROM room_questions WHERE image_id IS NOT NULL)`, cutoff)
	if err != nil {
		log.Printf("prune images failed: %v", err)
	}
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
