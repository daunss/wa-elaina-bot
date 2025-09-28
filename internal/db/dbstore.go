package db

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type ChatState struct {
	Persona string    // "elaina1" | "elaina2"
	Pro     bool      // mode pro ON/OFF
	Updated time.Time // audit
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS chat_state (
			jid TEXT PRIMARY KEY,
			persona TEXT NOT NULL DEFAULT 'elaina1',
			pro_mode INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		);
	`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Get(jid string) (ChatState, error) {
	row := s.db.QueryRow(`SELECT persona, pro_mode, updated_at FROM chat_state WHERE jid = ?`, jid)
	var persona string
	var pro int
	var ts int64
	def := ChatState{Persona: "elaina1", Pro: false, Updated: time.Unix(0, 0)}
	switch err := row.Scan(&persona, &pro, &ts); err {
	case nil:
		return ChatState{Persona: persona, Pro: pro == 1, Updated: time.Unix(ts, 0)}, nil
	case sql.ErrNoRows:
		return def, nil
	default:
		return def, err
	}
}

func (s *Store) SetPersona(jid, persona string) error {
	if persona != "elaina1" && persona != "elaina2" {
		return errors.New("invalid persona")
	}
	_, err := s.db.Exec(`
		INSERT INTO chat_state(jid, persona, pro_mode, updated_at)
		VALUES(?, ?, COALESCE((SELECT pro_mode FROM chat_state WHERE jid = ?),0), ?)
		ON CONFLICT(jid) DO UPDATE SET persona=excluded.persona, updated_at=excluded.updated_at
	`, jid, persona, jid, time.Now().Unix())
	return err
}

func (s *Store) SetPro(jid string, pro bool) error {
	v := 0
	if pro {
		v = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO chat_state(jid, persona, pro_mode, updated_at)
		VALUES(?, COALESCE((SELECT persona FROM chat_state WHERE jid = ?),'elaina1'), ?, ?)
		ON CONFLICT(jid) DO UPDATE SET pro_mode=excluded.pro_mode, updated_at=excluded.updated_at
	`, jid, jid, v, time.Now().Unix())
	return err
}
