package main

// Custom addition (fork lasse200188/multica, see CUSTOM.md): personal Telegram
// notifications.
//
// A user links their Telegram chat in Settings → Profile. From then on, every
// inbox item of type "mentioned" or "issue_assigned" addressed to them — in any
// workspace — is also sent to that chat by a dedicated notification bot.
//
// The inbox is the single source: recipient resolution, author skipping,
// @all/squad expansion and the per-workspace mute preferences all happen
// before the inbox item exists, so muting "Mentions" or "Assignments" in a
// workspace's Notifications settings silences Telegram as well.
//
// This is deliberately separate from the upstream Telegram integration
// (server/internal/integrations/telegram), which is a per-workspace agent chat
// bot: replies to that bot start agent runs.
//
// Linking: the profile page asks for a one-time token and opens
// https://t.me/<bot>?start=<token>. The bot long-polls getUpdates (outbound
// only, so it works on a private instance), receives "/start <token>" and
// stores the private chat id. "/stop" in the chat unlinks.
//
// MULTICA_CUSTOM_TELEGRAM_BOT_TOKEN   bot token from @BotFather (feature is
//                                     inactive without it)
// MULTICA_CUSTOM_TELEGRAM_NOTIFY=off  disables the feature

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	customTelegramNotifyEnv   = "MULTICA_CUSTOM_TELEGRAM_NOTIFY"
	customTelegramTokenEnv    = "MULTICA_CUSTOM_TELEGRAM_BOT_TOKEN"
	customTelegramAPIBase     = "https://api.telegram.org"
	customTelegramLinkTTL     = 15 * time.Minute
	customTelegramPollTimeout = 30 // seconds, Telegram long-poll timeout
	customTelegramQueueSize   = 256
	customTelegramSnippetMax  = 400 // runes of comment/description text
	customTelegramSendTimeout = 15 * time.Second
)

// customTelegram is the running feature, read by the HTTP routes. It is always
// non-nil after registerCustomTelegramNotify; configured=false means the
// routes report "not configured" and refuse to link.
var customTelegram *customTelegramNotify

type customTelegramNotify struct {
	configured bool
	pool       *pgxpool.Pool
	queries    *db.Queries
	api        *customTelegramBotAPI
	appURL     string
	queue      chan string // inbox item ids
	now        func() time.Time

	mu          sync.RWMutex
	botUsername string
}

func registerCustomTelegramNotify(bus *events.Bus, pool *pgxpool.Pool, queries *db.Queries) {
	s := &customTelegramNotify{pool: pool, queries: queries, now: time.Now}
	customTelegram = s
	switch strings.ToLower(strings.TrimSpace(os.Getenv(customTelegramNotifyEnv))) {
	case "off", "false", "0", "no":
		slog.Info("custom telegram notify disabled", "env", customTelegramNotifyEnv)
		return
	}
	token := strings.TrimSpace(os.Getenv(customTelegramTokenEnv))
	if token == "" {
		slog.Info("custom telegram notify inactive: bot token not set", "env", customTelegramTokenEnv)
		return
	}
	s.configured = true
	s.api = newCustomTelegramBotAPI(customTelegramAPIBase, token)
	s.appURL = appURLFromEnv()
	s.queue = make(chan string, customTelegramQueueSize)
	bus.Subscribe(protocol.EventInboxNew, s.onInboxNew)
	go s.deliverLoop()
	go s.pollLoop()
	slog.Info("custom telegram notify enabled")
}

func (s *customTelegramNotify) username() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.botUsername
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

// customTelegramWantsType reports whether an inbox item type is sent to
// Telegram at all.
func customTelegramWantsType(t string) bool {
	return t == "mentioned" || t == "issue_assigned"
}

// onInboxNew runs on the publisher's goroutine (the bus is synchronous, often
// inside an HTTP request), so it only filters and enqueues.
func (s *customTelegramNotify) onInboxNew(e events.Event) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	item, ok := payload["item"].(map[string]any)
	if !ok {
		return
	}
	if rt, _ := item["recipient_type"].(string); rt != "member" {
		return
	}
	if t, _ := item["type"].(string); !customTelegramWantsType(t) {
		return
	}
	id, _ := item["id"].(string)
	if id == "" {
		return
	}
	select {
	case s.queue <- id:
	default:
		slog.Warn("custom telegram notify: queue full, dropping notification", "inbox_item_id", id)
	}
}

func (s *customTelegramNotify) deliverLoop() {
	for id := range s.queue {
		ctx, cancel := context.WithTimeout(context.Background(), customTelegramSendTimeout)
		if err := s.deliver(ctx, id); err != nil {
			slog.Warn("custom telegram notify: delivery failed", "inbox_item_id", id, "error", err)
		}
		cancel()
	}
}

type customTelegramLink struct {
	ChatID            int64
	TelegramUsername  string
	NotifyMentions    bool
	NotifyAssignments bool
	LinkedAt          time.Time
}

func (s *customTelegramNotify) loadLink(ctx context.Context, userID pgtype.UUID) (customTelegramLink, bool, error) {
	var l customTelegramLink
	var username pgtype.Text
	err := s.pool.QueryRow(ctx, `
		SELECT chat_id, telegram_username, notify_mentions, notify_assignments, linked_at
		FROM custom_telegram_link WHERE user_id = $1`, userID).
		Scan(&l.ChatID, &username, &l.NotifyMentions, &l.NotifyAssignments, &l.LinkedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	if err != nil {
		return l, false, err
	}
	l.TelegramUsername = username.String
	return l, true, nil
}

func (s *customTelegramNotify) deliver(ctx context.Context, itemID string) error {
	id, err := util.ParseUUID(itemID)
	if err != nil {
		return err
	}
	item, err := s.queries.GetInboxItem(ctx, id)
	if err != nil {
		return fmt.Errorf("load inbox item: %w", err)
	}
	link, ok, err := s.loadLink(ctx, item.RecipientID)
	if err != nil || !ok {
		return err
	}
	switch item.Type {
	case "mentioned":
		if !link.NotifyMentions {
			return nil
		}
	case "issue_assigned":
		if !link.NotifyAssignments {
			return nil
		}
	default:
		return nil
	}

	msg := s.collectMessage(ctx, item)
	err = s.api.sendMessage(ctx, link.ChatID, buildCustomTelegramMessage(msg))
	var apiErr *customTelegramAPIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden {
		// The user blocked the bot or deleted the chat: unlink so we stop trying.
		_, _ = s.pool.Exec(ctx, `DELETE FROM custom_telegram_link WHERE user_id = $1 AND chat_id = $2`,
			item.RecipientID, link.ChatID)
		slog.Info("custom telegram notify: chat unreachable, link removed", "user_id", util.UUIDToString(item.RecipientID))
		return nil
	}
	return err
}

// customTelegramMessage is everything the message text is built from; kept
// separate from the DB lookups so the formatting is unit-testable.
type customTelegramMessage struct {
	Type          string // mentioned | issue_assigned
	WorkspaceName string
	Actor         string
	Identifier    string
	IssueTitle    string
	Status        string
	Priority      string
	Snippet       string // raw markdown, mentions still encoded
	Link          string
}

func (s *customTelegramNotify) collectMessage(ctx context.Context, item db.InboxItem) customTelegramMessage {
	m := customTelegramMessage{Type: item.Type, IssueTitle: item.Title}

	slug := ""
	prefix := ""
	if ws, err := s.queries.GetWorkspace(ctx, item.WorkspaceID); err == nil {
		m.WorkspaceName = ws.Name
		slug = ws.Slug
		prefix = ws.IssuePrefix
	}

	var description string
	if item.IssueID.Valid {
		if issue, err := s.queries.GetIssue(ctx, item.IssueID); err == nil {
			m.Identifier = service.IssueIdentifier(prefix, issue.Number)
			m.IssueTitle = issue.Title
			m.Status = issue.Status
			m.Priority = issue.Priority
			description = issue.Description.String
		}
	}
	if slug != "" {
		if m.Identifier != "" {
			m.Link = channel.IssueWebLink(s.appURL, slug, m.Identifier)
		} else if s.appURL != "" {
			m.Link = strings.TrimRight(s.appURL, "/") + "/" + slug + "/inbox"
		}
	}

	m.Actor = s.actorName(ctx, item.ActorType.String, item.ActorID)

	if item.Type == "mentioned" {
		var details struct {
			CommentID string `json:"comment_id"`
		}
		_ = json.Unmarshal(item.Details, &details)
		if details.CommentID != "" {
			if cid, err := util.ParseUUID(details.CommentID); err == nil {
				if c, err := s.queries.GetComment(ctx, cid); err == nil {
					m.Snippet = customTelegramSnippet(c.Content, util.UUIDToString(item.RecipientID))
				}
			}
		} else if description != "" {
			m.Snippet = customTelegramSnippet(description, util.UUIDToString(item.RecipientID))
		}
	}
	return m
}

func (s *customTelegramNotify) actorName(ctx context.Context, actorType string, actorID pgtype.UUID) string {
	if !actorID.Valid {
		if actorType == "system" {
			return "Multica"
		}
		return ""
	}
	switch actorType {
	case "member":
		if u, err := s.queries.GetUser(ctx, actorID); err == nil {
			return u.Name
		}
	case "agent":
		if a, err := s.queries.GetAgent(ctx, actorID); err == nil {
			return a.Name
		}
	}
	return ""
}

// customTelegramSnippet picks the paragraph that mentions the recipient (or
// @all), falling back to the first paragraph.
func customTelegramSnippet(content, recipientID string) string {
	paragraphs := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n\n")
	pick := ""
	for _, p := range paragraphs {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if pick == "" {
			pick = p
		}
		if (recipientID != "" && strings.Contains(p, "/"+recipientID+")")) || strings.Contains(p, "mention://all/") {
			pick = p
			break
		}
	}
	return strings.TrimSpace(pick)
}

// customTelegramRenderText turns markdown mentions into "@Label" and cuts the
// text to max runes.
func customTelegramRenderText(s string, max int) string {
	s = util.MentionRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := util.MentionRe.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		if sub[2] == "issue" {
			return sub[1]
		}
		return "@" + strings.TrimPrefix(sub[1], "@")
	})
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}

var customTelegramStatusLabels = map[string]string{
	"backlog":     "Backlog",
	"todo":        "Todo",
	"in_progress": "In Arbeit",
	"in_review":   "Im Review",
	"blocked":     "Blockiert",
	"done":        "Erledigt",
	"cancelled":   "Abgebrochen",
}

var customTelegramPriorityLabels = map[string]string{
	"urgent": "Dringend",
	"high":   "Hoch",
	"medium": "Mittel",
	"low":    "Niedrig",
}

// buildCustomTelegramMessage renders the Telegram HTML message. Every piece of
// user-written text is escaped.
func buildCustomTelegramMessage(m customTelegramMessage) string {
	esc := html.EscapeString
	var b strings.Builder

	if m.WorkspaceName != "" {
		b.WriteString("🏢 <b>" + esc(m.WorkspaceName) + "</b>\n")
	}

	issue := ""
	if m.Identifier != "" {
		issue = "<b>" + esc(m.Identifier) + "</b> "
	}
	issue += "„" + esc(m.IssueTitle) + "“"

	actor := esc(m.Actor)
	switch m.Type {
	case "issue_assigned":
		if actor != "" {
			b.WriteString("👤 " + actor + " hat dir " + issue + " zugewiesen")
		} else {
			b.WriteString("👤 Dir wurde " + issue + " zugewiesen")
		}
		var meta []string
		if m.Status != "" {
			label := customTelegramStatusLabels[m.Status]
			if label == "" {
				label = m.Status
			}
			meta = append(meta, "Status: "+esc(label))
		}
		if label := customTelegramPriorityLabels[m.Priority]; label != "" {
			meta = append(meta, "Priorität: "+esc(label))
		}
		if len(meta) > 0 {
			b.WriteString("\n" + strings.Join(meta, " · "))
		}
	default:
		if actor != "" {
			b.WriteString("📣 " + actor + " hat dich in " + issue + " erwähnt")
		} else {
			b.WriteString("📣 Du wurdest in " + issue + " erwähnt")
		}
		if text := customTelegramRenderText(m.Snippet, customTelegramSnippetMax); text != "" {
			b.WriteString("\n\n<blockquote>" + esc(text) + "</blockquote>")
		}
	}

	if m.Link != "" {
		b.WriteString("\n\n<a href=\"" + esc(m.Link) + "\">In Multica öffnen</a>")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Linking
// ---------------------------------------------------------------------------

func customTelegramHashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// mintLinkToken creates a single-use token for userID and returns the t.me
// deep link. Older tokens of the user and expired tokens are removed.
func (s *customTelegramNotify) mintLinkToken(ctx context.Context, userID pgtype.UUID) (string, time.Time, error) {
	bot := s.username()
	if bot == "" {
		return "", time.Time{}, errors.New("telegram bot not ready yet")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	raw := base64.RawURLEncoding.EncodeToString(buf) // 43 chars of [A-Za-z0-9_-], valid start payload
	expires := s.now().Add(customTelegramLinkTTL)
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM custom_telegram_link_token WHERE user_id = $1 OR expires_at < now()`, userID); err != nil {
		return "", time.Time{}, err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO custom_telegram_link_token (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		customTelegramHashToken(raw), userID, expires); err != nil {
		return "", time.Time{}, err
	}
	return "https://t.me/" + bot + "?start=" + raw, expires, nil
}

// redeemLinkToken consumes a token and links chatID to its user. Returns the
// user's name for the confirmation message.
func (s *customTelegramNotify) redeemLinkToken(ctx context.Context, raw string, chatID int64, username string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID pgtype.UUID
	err = tx.QueryRow(ctx, `
		DELETE FROM custom_telegram_link_token
		WHERE token_hash = $1 AND expires_at > $2
		RETURNING user_id`, customTelegramHashToken(raw), s.now()).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errCustomTelegramTokenInvalid
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO custom_telegram_link (user_id, chat_id, telegram_username, linked_at)
		VALUES ($1, $2, NULLIF($3, ''), now())
		ON CONFLICT (user_id) DO UPDATE
		SET chat_id = EXCLUDED.chat_id, telegram_username = EXCLUDED.telegram_username, linked_at = now()`,
		userID, chatID, username); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	name := ""
	if u, err := s.queries.GetUser(ctx, userID); err == nil {
		name = u.Name
	}
	return name, nil
}

var errCustomTelegramTokenInvalid = errors.New("link token invalid or expired")

// ---------------------------------------------------------------------------
// Bot updates (long polling)
// ---------------------------------------------------------------------------

func (s *customTelegramNotify) pollLoop() {
	ctx := context.Background()
	backoff := 5 * time.Second
	// Resolve the bot's username (needed for deep links) and make sure no
	// webhook is set, otherwise getUpdates is refused.
	for {
		me, err := s.api.getMe(ctx)
		if err == nil {
			s.mu.Lock()
			s.botUsername = me.Username
			s.mu.Unlock()
			if err := s.api.deleteWebhook(ctx); err != nil {
				slog.Warn("custom telegram notify: deleteWebhook failed", "error", err)
			}
			slog.Info("custom telegram notify: bot ready", "bot", me.Username)
			break
		}
		slog.Warn("custom telegram notify: getMe failed, retrying", "error", err)
		time.Sleep(backoff)
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}

	var offset int64
	backoff = 5 * time.Second
	for {
		updates, err := s.api.getUpdates(ctx, offset, customTelegramPollTimeout)
		if err != nil {
			slog.Warn("custom telegram notify: getUpdates failed", "error", err)
			time.Sleep(backoff)
			if backoff < 2*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message != nil {
				s.handleMessage(ctx, u.Message)
			}
		}
	}
}

func (s *customTelegramNotify) handleMessage(parent context.Context, m *customTelegramBotMessage) {
	if m.Chat.Type != "private" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, customTelegramSendTimeout)
	defer cancel()

	reply := func(text string) {
		if err := s.api.sendMessage(ctx, m.Chat.ID, text); err != nil {
			slog.Warn("custom telegram notify: reply failed", "error", err)
		}
	}
	help := "Hier bekommst du Nachrichten von Multica, wenn du in einem Issue erwähnt wirst oder dir ein Issue zugewiesen wird.\n\n" +
		"Zum Verbinden: in Multica unter <b>Einstellungen → Profil → Telegram</b> auf „Connect Telegram“ klicken.\n" +
		"/stop trennt die Verbindung."

	fields := strings.Fields(m.Text)
	if len(fields) == 0 {
		reply(help)
		return
	}
	cmd := strings.ToLower(fields[0])
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	switch cmd {
	case "/start":
		if len(fields) < 2 {
			reply(help)
			return
		}
		username := ""
		if m.From != nil {
			username = m.From.Username
		}
		name, err := s.redeemLinkToken(ctx, fields[1], m.Chat.ID, username)
		if errors.Is(err, errCustomTelegramTokenInvalid) {
			reply("Der Link ist abgelaufen oder wurde schon benutzt. Bitte in Multica unter Einstellungen → Profil erneut „Connect Telegram“ klicken.")
			return
		}
		if err != nil {
			slog.Warn("custom telegram notify: redeem failed", "error", err)
			reply("Das Verbinden hat nicht geklappt. Bitte später noch einmal versuchen.")
			return
		}
		who := ""
		if name != "" {
			who = " <b>" + html.EscapeString(name) + "</b>"
		}
		reply("✅ Verbunden mit dem Multica-Konto" + who + ".\n\n" +
			"Du bekommst hier eine Nachricht, wenn du in einem Issue erwähnt wirst oder dir ein Issue zugewiesen wird – aus allen Workspaces.\n" +
			"/stop trennt die Verbindung.")
	case "/stop":
		tag, err := s.pool.Exec(ctx, `DELETE FROM custom_telegram_link WHERE chat_id = $1`, m.Chat.ID)
		if err != nil {
			slog.Warn("custom telegram notify: unlink failed", "error", err)
			reply("Das Trennen hat nicht geklappt. Bitte später noch einmal versuchen.")
			return
		}
		if tag.RowsAffected() == 0 {
			reply("Dieser Chat ist mit keinem Multica-Konto verbunden.")
			return
		}
		reply("Verbindung getrennt. Du bekommst hier keine Nachrichten mehr.")
	default:
		reply(help)
	}
}

// ---------------------------------------------------------------------------
// Minimal Bot API client
// ---------------------------------------------------------------------------

type customTelegramBotAPI struct {
	base   string
	token  string
	client *http.Client
}

func newCustomTelegramBotAPI(base, token string) *customTelegramBotAPI {
	// The client timeout has to outlast the long-poll timeout.
	return &customTelegramBotAPI{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		client: &http.Client{Timeout: time.Duration(customTelegramPollTimeout+15) * time.Second},
	}
}

type customTelegramAPIError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *customTelegramAPIError) Error() string {
	return fmt.Sprintf("telegram api %d: %s", e.Code, e.Description)
}

type customTelegramBotUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type customTelegramBotMessage struct {
	MessageID int64                  `json:"message_id"`
	From      *customTelegramBotUser `json:"from"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text string `json:"text"`
}

type customTelegramUpdate struct {
	UpdateID int64                     `json:"update_id"`
	Message  *customTelegramBotMessage `json:"message"`
}

func (a *customTelegramBotAPI) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/bot"+a.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return a.scrub(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		// *url.Error carries the request URL, which contains the token.
		return a.scrub(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return a.scrub(err)
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram api %s: http %d, invalid response", method, resp.StatusCode)
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &customTelegramAPIError{Code: code, Description: env.Description, RetryAfter: env.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (a *customTelegramBotAPI) scrub(err error) error {
	if a.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), a.token, "<token>"))
}

func (a *customTelegramBotAPI) getMe(ctx context.Context) (customTelegramBotUser, error) {
	var u customTelegramBotUser
	err := a.call(ctx, "getMe", struct{}{}, &u)
	return u, err
}

func (a *customTelegramBotAPI) deleteWebhook(ctx context.Context) error {
	return a.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

func (a *customTelegramBotAPI) getUpdates(ctx context.Context, offset int64, timeout int) ([]customTelegramUpdate, error) {
	var updates []customTelegramUpdate
	err := a.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message"},
	}, &updates)
	return updates, err
}

// sendMessage sends an HTML message, waiting out one rate limit if needed.
func (a *customTelegramBotAPI) sendMessage(ctx context.Context, chatID int64, text string) error {
	params := map[string]any{
		"chat_id":              chatID,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	err := a.call(ctx, "sendMessage", params, nil)
	var apiErr *customTelegramAPIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusTooManyRequests && apiErr.RetryAfter > 0 && apiErr.RetryAfter <= 10 {
		select {
		case <-time.After(time.Duration(apiErr.RetryAfter) * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
		err = a.call(ctx, "sendMessage", params, nil)
	}
	return err
}
