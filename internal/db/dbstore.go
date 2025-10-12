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

type PeraturanState struct {
	Enabled bool
	Rules   string
	Updated time.Time
}

type WarnRecord struct {
	Group      string
	User       string
	Count      int
	LastReason string
	Updated    time.Time
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
		CREATE TABLE IF NOT EXISTS peraturan_state (
			group_jid TEXT PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0,
			rules TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS peraturan_warn (
			group_jid TEXT NOT NULL,
			user_jid TEXT NOT NULL,
			warns INTEGER NOT NULL DEFAULT 0,
			last_reason TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (group_jid, user_jid)
		);
		CREATE INDEX IF NOT EXISTS idx_peraturan_warn_group ON peraturan_warn(group_jid);
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

func (s *Store) GetPeraturanState(group string) (PeraturanState, error) {
	row := s.db.QueryRow(`SELECT enabled, rules, updated_at FROM peraturan_state WHERE group_jid = ?`, group)
	var enabled sql.NullInt64
	var rules sql.NullString
	var ts sql.NullInt64
	def := PeraturanState{Enabled: false, Rules: "", Updated: time.Unix(0, 0)}
	switch err := row.Scan(&enabled, &rules, &ts); err {
	case nil:
		return PeraturanState{
			Enabled: enabled.Int64 == 1,
			Rules:   rules.String,
			Updated: time.Unix(ts.Int64, 0),
		}, nil
	case sql.ErrNoRows:
		return def, nil
	default:
		return def, err
	}
}

func (s *Store) SetPeraturanState(group string, enabled bool, rules string) error {
	flag := 0
	if enabled {
		flag = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO peraturan_state(group_jid, enabled, rules, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(group_jid) DO UPDATE SET
			enabled = excluded.enabled,
			rules = excluded.rules,
			updated_at = excluded.updated_at
	`, group, flag, rules, time.Now().Unix())
	return err
}

func (s *Store) GetWarn(group, user string) (WarnRecord, error) {
	row := s.db.QueryRow(`SELECT warns, last_reason, updated_at FROM peraturan_warn WHERE group_jid = ? AND user_jid = ?`, group, user)
	var warns int
	var reason string
	var ts int64
	switch err := row.Scan(&warns, &reason, &ts); err {
	case nil:
		return WarnRecord{
			Group:      group,
			User:       user,
			Count:      warns,
			LastReason: reason,
			Updated:    time.Unix(ts, 0),
		}, nil
	case sql.ErrNoRows:
		return WarnRecord{Group: group, User: user}, nil
	default:
		return WarnRecord{}, err
	}
}

func (s *Store) AddWarn(group, user, reason string) (WarnRecord, error) {
	now := time.Now().Unix()
	row := s.db.QueryRow(`
		INSERT INTO peraturan_warn(group_jid, user_jid, warns, last_reason, updated_at)
		VALUES(?, ?, 1, ?, ?)
		ON CONFLICT(group_jid, user_jid) DO UPDATE SET
			warns = peraturan_warn.warns + 1,
			last_reason = excluded.last_reason,
			updated_at = excluded.updated_at
		RETURNING warns, last_reason, updated_at
	`, group, user, reason, now)
	var warns int
	var last string
	var ts int64
	if err := row.Scan(&warns, &last, &ts); err != nil {
		return WarnRecord{}, err
	}
	return WarnRecord{
		Group:      group,
		User:       user,
		Count:      warns,
		LastReason: last,
		Updated:    time.Unix(ts, 0),
	}, nil
}

func (s *Store) DecrementWarn(group, user string) (WarnRecord, error) {
	now := time.Now().Unix()
	row := s.db.QueryRow(`
		UPDATE peraturan_warn
		SET
			warns = CASE WHEN warns <= 1 THEN 0 ELSE warns - 1 END,
			last_reason = CASE WHEN warns <= 1 THEN '' ELSE last_reason END,
			updated_at = ?
		WHERE group_jid = ? AND user_jid = ?
		RETURNING warns, last_reason, updated_at
	`, now, group, user)
	var warns int
	var reason string
	var ts int64
	switch err := row.Scan(&warns, &reason, &ts); err {
	case sql.ErrNoRows:
		return WarnRecord{Group: group, User: user}, nil
	case nil:
		rec := WarnRecord{
			Group:      group,
			User:       user,
			Count:      warns,
			LastReason: reason,
			Updated:    time.Unix(ts, 0),
		}
		if warns <= 0 {
			_, _ = s.db.Exec(`DELETE FROM peraturan_warn WHERE group_jid = ? AND user_jid = ?`, group, user)
			rec.Count = 0
			rec.LastReason = ""
			rec.Updated = time.Now()
		}
		return rec, nil
	default:
		return WarnRecord{}, err
	}
}

func (s *Store) ClearWarns(group, user string) error {
	_, err := s.db.Exec(`DELETE FROM peraturan_warn WHERE group_jid = ? AND user_jid = ?`, group, user)
	return err
}

func (s *Store) ListWarns(group string) ([]WarnRecord, error) {
	rows, err := s.db.Query(`SELECT user_jid, warns, last_reason, updated_at FROM peraturan_warn WHERE group_jid = ? ORDER BY warns DESC, updated_at DESC`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WarnRecord
	for rows.Next() {
		var user string
		var warns int
		var reason string
		var ts int64
		if err := rows.Scan(&user, &warns, &reason, &ts); err != nil {
			return nil, err
		}
		out = append(out, WarnRecord{
			Group:      group,
			User:       user,
			Count:      warns,
			LastReason: reason,
			Updated:    time.Unix(ts, 0),
		})
	}
	return out, rows.Err()
}
