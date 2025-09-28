package memory

import (
	"database/sql"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Turn struct {
	Role string // "user" | "assistant"
	Text string
	TS   time.Time
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func maxTurns() int {
	n, _ := strconv.Atoi(strings.TrimSpace(getenv("MEMORY_TURNS", "8")))
	if n < 0 { n = 0 }
	if n > 30 { n = 30 }
	return n
}

func charBudget() int {
	n, _ := strconv.Atoi(strings.TrimSpace(getenv("MEMORY_CHAR_BUDGET", "4000")))
	if n < 500 { n = 500 }
	if n > 20000 { n = 20000 }
	return n
}

func openDB() (*sql.DB, error) {
	// Pakai file STATE_DB yang sudah kamu gunakan untuk chat_state
	path := getenv("STATE_DB", "elaina_state.db")
	return sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS chat_memory (
  jid TEXT NOT NULL,
  role TEXT NOT NULL,       -- 'user' | 'assistant'
  text TEXT NOT NULL,
  ts   INTEGER NOT NULL,
  PRIMARY KEY (jid, ts, role, text)
);
CREATE INDEX IF NOT EXISTS chat_memory_idx ON chat_memory(jid, ts);
`)
	return err
}

// SaveTurn menyimpan satu giliran (user/assistant)
func SaveTurn(jid, role, text string) error {
	if strings.TrimSpace(jid) == "" || strings.TrimSpace(role) == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	db, err := openDB()
	if err != nil { return err }
	defer db.Close()
	if err := migrate(db); err != nil { return err }

	_, err = db.Exec(`INSERT OR IGNORE INTO chat_memory(jid, role, text, ts) VALUES(?,?,?,?)`,
		jid, role, text, time.Now().Unix())
	return err
}

// Load mengambil N*2 turn terakhir (user+assistant) untuk JID
func Load(jid string) ([]Turn, error) {
	db, err := openDB()
	if err != nil { return nil, err }
	defer db.Close()
	if err := migrate(db); err != nil { return nil, err }

	N := maxTurns()
	if N == 0 { return nil, nil }

	rows, err := db.Query(`SELECT role, text, ts FROM chat_memory WHERE jid=? ORDER BY ts DESC LIMIT ?`, jid, N*2)
	if err != nil { return nil, err }
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var t Turn
		var ts int64
		if err := rows.Scan(&t.Role, &t.Text, &ts); err == nil {
			t.TS = time.Unix(ts, 0)
			out = append(out, t)
		}
	}
	// Balikkan ke urutan kronologis
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// BuildContext merangkai memory + userText jadi satu prompt hemat token
func BuildContext(hist []Turn, userText string) string {
	if len(hist) == 0 {
		return userText
	}
	var b strings.Builder
	b.WriteString("Konteks percakapan sebelumnya (ringkas):\n")
	limit := charBudget()
	for _, t := range hist {
		line := ""
		if t.Role == "assistant" {
			line = "Elaina: " + t.Text
		} else {
			line = "User: " + t.Text
		}
		if b.Len()+len(line)+1 > limit { break }
		b.WriteString(line)
		if !strings.HasSuffix(line, "\n") { b.WriteByte('\n') }
	}
	b.WriteString("\nPertanyaan/teks baru:\n")
	b.WriteString(userText)
	return b.String()
}
