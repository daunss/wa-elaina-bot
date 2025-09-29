package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type Turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

var (
	mu          sync.RWMutex
	chatHistMap = make(map[string][]Turn)
	userNameMap = make(map[string]string) // key: senderJID, value: nama
	maxTurns    = 8
	
	reNameRequest = regexp.MustCompile(`(?i)^(panggil aku|sebut aku|nama aku|namaku|name is|call me)\s+(.+)$`)
)

func Load(chatJID string) ([]Turn, error) {
	mu.RLock()
	defer mu.RUnlock()
	hist, ok := chatHistMap[chatJID]
	if !ok {
		return nil, nil
	}
	return hist, nil
}

func SaveTurn(chatJID, role, text string) error {
	mu.Lock()
	defer mu.Unlock()
	
	hist := chatHistMap[chatJID]
	hist = append(hist, Turn{Role: role, Text: text})
	
	if len(hist) > maxTurns*2 {
		hist = hist[len(hist)-(maxTurns*2):]
	}
	
	chatHistMap[chatJID] = hist
	return persistChat(chatJID, hist)
}

func BuildContext(hist []Turn, newUserText, senderJID string) string {
	var sb strings.Builder
	
	userName, exists := GetUserName(senderJID)
	if !exists || userName == "" {
		userName = "kamu"
	}
	
	if len(hist) > 0 {
		sb.WriteString("Konteks percakapan sebelumnya:\n")
		for _, t := range hist {
			if t.Role == "user" {
				sb.WriteString("User: " + t.Text + "\n")
			} else {
				sb.WriteString("Elaina: " + t.Text + "\n")
			}
		}
		sb.WriteString("\n")
	}
	
	sb.WriteString("Nama pengguna: " + userName + "\n\n")
	
	sb.WriteString("Pertanyaan/teks baru:\n")
	sb.WriteString(newUserText)
	
	return sb.String()
}

func DetectNameRequest(text string) (string, bool) {
	text = strings.TrimSpace(text)
	matches := reNameRequest.FindStringSubmatch(text)
	if len(matches) >= 3 {
		name := strings.TrimSpace(matches[2])
		name = strings.Trim(name, `"'.!`)
		if name != "" {
			return name, true
		}
	}
	return "", false
}

func SetUserName(senderJID, name string) error {
	mu.Lock()
	defer mu.Unlock()
	
	userNameMap[senderJID] = name
	return persistUserNames()
}

func GetUserName(senderJID string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	
	name, ok := userNameMap[senderJID]
	return name, ok
}

func persistChat(chatJID string, hist []Turn) error {
	dir := "data/memory"
	os.MkdirAll(dir, 0755)
	
	path := filepath.Join(dir, sanitize(chatJID)+".json")
	data, _ := json.MarshalIndent(hist, "", "  ")
	return os.WriteFile(path, data, 0644)
}

func persistUserNames() error {
	dir := "data/memory"
	os.MkdirAll(dir, 0755)
	
	path := filepath.Join(dir, "_usernames.json")
	data, _ := json.MarshalIndent(userNameMap, "", "  ")
	return os.WriteFile(path, data, 0644)
}

func LoadAll() {
	mu.Lock()
	defer mu.Unlock()
	
	loadUserNames()
	
	dir := "data/memory"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && e.Name() != "_usernames.json" {
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			
			var hist []Turn
			if json.Unmarshal(data, &hist) == nil {
				chatJID := strings.TrimSuffix(e.Name(), ".json")
				chatHistMap[chatJID] = hist
			}
		}
	}
}

func loadUserNames() {
	path := filepath.Join("data/memory", "_usernames.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	
	json.Unmarshal(data, &userNameMap)
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "@", "_")
	s = strings.ReplaceAll(s, ":", "_")
	return s
}