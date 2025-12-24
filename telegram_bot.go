package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/celestix/gotgproto/storage"
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
)

// ============================================================================
// 1. CONFIGURACIÓN
// ============================================================================

type Configuration struct {
	ApiID          int
	ApiHash        string
	BotToken       string
	DatabasePath   string
	CacheDirectory string
	BaseURL        string
	Port           string
	HashLength     int
	DebugMode      bool
	GitHubToken    string
	GitHubOwner    string
	GitHubRepo     string
	GitHubDBPath   string
}

func LoadConfig() *Configuration {
	godotenv.Load()
	apiID, _ := strconv.Atoi(os.Getenv("API_ID"))
	hashLen, _ := strconv.Atoi(os.Getenv("HASH_LENGTH"))
	if hashLen == 0 { hashLen = 12 }

	return &Configuration{
		ApiID:          apiID,
		ApiHash:        os.Getenv("API_HASH"),
		BotToken:       os.Getenv("BOT_TOKEN"),
		DatabasePath:   "session.sqlite",
		CacheDirectory: ".cache",
		BaseURL:        os.Getenv("BASE_URL"),
		Port:           os.Getenv("PORT"),
		HashLength:     hashLen,
		DebugMode:      os.Getenv("DEBUG_MODE") == "true",
		GitHubOwner:    os.Getenv("GITHUB_OWNER"),
		GitHubRepo:     os.Getenv("GITHUB_REPO"),
		GitHubDBPath:   os.Getenv("GITHUB_DB_PATH"),
	}
}

// ============================================================================
// 2. BASE DE DATOS JSON & GITHUB SYNC
// ============================================================================

type UserRecord struct {
	UserID       int64  `json:"user_id"`
	ChatID       int64  `json:"chat_id"`
	FirstName    string `json:"first_name"`
	Username     string `json:"username"`
	IsAuthorized bool   `json:"is_authorized"`
	IsAdmin      bool   `json:"is_admin"`
	CreatedAt    string `json:"created_at"`
}

type FileRecord struct {
	ID        string `json:"id"`
	UserID    int64  `json:"user_id"`
	MessageID int    `json:"message_id"`
	FileName  string `json:"file_name"`
	MimeType  string `json:"mime_type"`
	FileSize  int64  `json:"file_size"`
	RemoteURL string `json:"remote_url"`
	CreatedAt string `json:"created_at"`
}

type localData struct {
	Users    map[int64]*UserRecord `json:"users"`
	Files    map[string]*FileRecord `json:"files"`
	Settings map[string]string     `json:"settings"`
	Bans     map[int64]string      `json:"bans"`
}

type LocalJSONDB struct {
	mu   sync.RWMutex
	path string
	data *localData
}

func NewLocalJSONDB(path string) (*LocalJSONDB, error) {
	l := &LocalJSONDB{path: path}
	if err := l.load(); err != nil {
		if os.IsNotExist(err) {
			l.data = &localData{
				Users:    make(map[int64]*UserRecord),
				Files:    make(map[string]*FileRecord),
				Settings: make(map[string]string),
				Bans:     make(map[int64]string),
			}
			l.save()
			return l, nil
		}
		return nil, err
	}
	return l, nil
}

func (l *LocalJSONDB) load() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil { return err }
	defer f.Close()
	l.data = &localData{}
	return json.NewDecoder(f).Decode(l.data)
}

func (l *LocalJSONDB) save() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	os.MkdirAll(filepath.Dir(l.path), 0755)
	b, _ := json.MarshalIndent(l.data, "", "  ")
	return os.WriteFile(l.path, b, 0644)
}

// ============================================================================
// 3. LOGIC & HANDLERS
// ============================================================================

type TelegramBot struct {
	config    *Configuration
	tgClient  *gotgproto.Client
	tgCtx     *ext.Context
	localDB   *LocalJSONDB
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStart))
	d.AddHandler(handlers.NewCommand("files", b.handleFiles))
	d.AddHandler(handlers.NewCommand("dbtoken", b.handleDBToken))
	d.AddHandler(handlers.NewCommand("dbsync", b.handleDBSync))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMedia))
}

func (b *TelegramBot) handleStart(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	b.localDB.mu.Lock()
	if _, ok := b.localDB.data.Users[user.ID]; !ok {
		b.localDB.data.Users[user.ID] = &UserRecord{
			UserID: user.ID, ChatID: user.ID, FirstName: user.FirstName, Username: user.Username,
			IsAuthorized: true, CreatedAt: time.Now().Format(time.RFC3339),
			IsAdmin: len(b.localDB.data.Users) == 0,
		}
		b.localDB.mu.Unlock()
		b.localDB.save()
	} else { b.localDB.mu.Unlock() }

	msg := fmt.Sprintf("Welcome to Host Wave, %s!\nSend me any file to generate a stream link.", user.FirstName)
	_, err := ctx.Reply(u, msg, nil)
	return err
}

func (b *TelegramBot) handleMedia(ctx *ext.Context, u *ext.Update) error {
	msg := u.EffectiveMessage
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok { return nil }
	doc, ok := media.Document.(*tg.Document)
	if !ok { return nil }

	hash := GetShortHash(fmt.Sprintf("%d%d", doc.ID, time.Now().UnixNano()), b.config.HashLength)
	fname := "file"
	for _, a := range doc.Attributes {
		if x, ok := a.(*tg.DocumentAttributeFilename); ok { fname = x.FileName }
	}

	link := fmt.Sprintf("%s/stream/%s", b.config.BaseURL, hash)
	
	b.localDB.mu.Lock()
	b.localDB.data.Files[hash] = &FileRecord{
		ID: hash, UserID: u.EffectiveUser().ID, MessageID: msg.ID,
		FileName: fname, FileSize: doc.Size, MimeType: doc.MimeType,
		RemoteURL: link, CreatedAt: time.Now().Format(time.RFC3339),
	}
	b.localDB.mu.Unlock()
	b.localDB.save()

	resp := fmt.Sprintf("📄 **File Uploaded**\n\nName: %s\nSize: %s\nType: %s\n\n🔗 Link: %s", 
		fname, humanBytes(doc.Size), doc.MimeType, link)
	
	ctx.Reply(u, resp, nil)
	go b.performDBSync("auto-sync: upload")
	return nil
}

func (b *TelegramBot) handleFiles(ctx *ext.Context, u *ext.Update) error {
	uid := u.EffectiveUser().ID
	var sb strings.Builder
	sb.WriteString("📂 **Your Files:**\n\n")
	
	b.localDB.mu.RLock()
	for _, f := range b.localDB.data.Files {
		if f.UserID == uid {
			sb.WriteString(fmt.Sprintf("• %s\n  %s\n", f.FileName, f.RemoteURL))
		}
	}
	b.localDB.mu.RUnlock()
	
	ctx.Reply(u, sb.String(), &gotgproto.SendOpts{LinkPreview: false})
	return nil
}

func (b *TelegramBot) handleDBToken(ctx *ext.Context, u *ext.Update) error {
	args := strings.Fields(u.EffectiveMessage.Message)
	if len(args) < 2 { return nil }
	b.localDB.mu.Lock()
	b.localDB.data.Settings["github_token"] = args[1]
	b.localDB.mu.Unlock()
	b.localDB.save()
	ctx.Reply(u, "✅ GitHub Token updated.", nil)
	return nil
}

func (b *TelegramBot) handleDBSync(ctx *ext.Context, u *ext.Update) error {
	err := b.performDBSync("manual sync")
	if err != nil { ctx.Reply(u, "❌ Sync failed: "+err.Error(), nil) }
	else { ctx.Reply(u, "✅ Sync successful.", nil) }
	return nil
}

func (b *TelegramBot) performDBSync(msg string) error {
	b.localDB.mu.RLock()
	token := b.localDB.data.Settings["github_token"]
	content, _ := json.MarshalIndent(b.localDB.data, "", "  ")
	b.localDB.mu.RUnlock()

	if token == "" { return errors.New("token missing") }

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", 
		b.config.GitHubOwner, b.config.GitHubRepo, b.config.GitHubDBPath)
	
	client := &http.Client{Timeout: 20 * time.Second}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "token "+token)
	resp, _ := client.Do(req)
	
	var sha string
	if resp.StatusCode == 200 {
		var res map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&res)
		sha = res["sha"].(string)
	}

	payload := map[string]interface{}{
		"message": msg, "content": base64.StdEncoding.EncodeToString(content),
	}
	if sha != "" { payload["sha"] = sha }
	
	body, _ := json.Marshal(payload)
	reqPut, _ := http.NewRequest("PUT", url, bytes.NewBuffer(body))
	reqPut.Header.Set("Authorization", "token "+token)
	respPut, err := client.Do(reqPut)
	if err != nil { return err }
	if respPut.StatusCode < 200 || respPut.StatusCode >= 300 { return errors.New("gh api error") }
	return nil
}

// ============================================================================
// 4. MAIN & UTILS
// ============================================================================

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit { return fmt.Sprintf("%d B", b) }
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func GetShortHash(input string, length int) string {
	h := md5.Sum([]byte(input))
	return hex.EncodeToString(h[:])[:length]
}

func main() {
	cfg := LoadConfig()
	db, _ := NewLocalJSONDB("storage/database.json")
	
	client, err := gotgproto.NewClient(cfg.ApiID, cfg.ApiHash, 
		gotgproto.ClientTypeBot(cfg.BotToken), 
		&gotgproto.ClientOpts{
			Session: sessionMaker.SqlSession(sqlite.Open(cfg.DatabasePath)),
		})
	if err != nil { log.Fatal(err) }

	bot := &TelegramBot{config: cfg, tgClient: client, localDB: db, tgCtx: client.CreateContext()}
	bot.registerHandlers()

	log.Println("🚀 Host Wave Bot is running...")
	client.Idle()
}
