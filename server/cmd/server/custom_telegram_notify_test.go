package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// fakeTelegram is a minimal Bot API stand-in that records sendMessage calls.
type fakeTelegram struct {
	mu       sync.Mutex
	sent     []map[string]any
	sendCode int // non-zero: answer sendMessage with this error code
	calls    map[string]int
	srv      *httptest.Server
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bottest-token/") {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/bottest-token/")
		body, _ := io.ReadAll(r.Body)
		var params map[string]any
		_ = json.Unmarshal(body, &params)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[method]++
		switch method {
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"username":"multica_test_bot"}}`)
		case "sendMessage":
			if f.sendCode != 0 {
				w.WriteHeader(f.sendCode)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":`+itoa(f.sendCode)+`,"description":"Forbidden: bot was blocked by the user"}`)
				return
			}
			f.sent = append(f.sent, params)
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (f *fakeTelegram) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

func TestBuildCustomTelegramMessageMention(t *testing.T) {
	got := buildCustomTelegramMessage(customTelegramMessage{
		Type:          "mentioned",
		WorkspaceName: "Acme <Labs>",
		Actor:         "Coder",
		Identifier:    "MUL-12",
		IssueTitle:    "Fix <script> & stuff",
		Snippet:       "Hey [@Jens](mention://member/11111111-1111-1111-1111-111111111111), see [MUL-3](mention://issue/22222222-2222-2222-2222-222222222222)",
		Link:          "https://multica.example/acme/issues/MUL-12",
	})
	for _, want := range []string{
		"🏢 <b>Acme &lt;Labs&gt;</b>\n",
		"📣 Coder hat dich in <b>MUL-12</b> „Fix &lt;script&gt; &amp; stuff“ erwähnt",
		"<blockquote>Hey @Jens, see MUL-3</blockquote>",
		`<a href="https://multica.example/acme/issues/MUL-12">In Multica öffnen</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "mention://") {
		t.Errorf("raw mention markdown leaked:\n%s", got)
	}
}

func TestBuildCustomTelegramMessageAssignment(t *testing.T) {
	got := buildCustomTelegramMessage(customTelegramMessage{
		Type:          "issue_assigned",
		WorkspaceName: "Acme",
		Actor:         "Anna",
		Identifier:    "MUL-7",
		IssueTitle:    "Ship it",
		Status:        "in_progress",
		Priority:      "high",
		Snippet:       "ignored for assignments",
	})
	for _, want := range []string{
		"🏢 <b>Acme</b>\n👤 Anna hat dir <b>MUL-7</b> „Ship it“ zugewiesen",
		"Status: In Arbeit · Priorität: Hoch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ignored") || strings.Contains(got, "<a href") {
		t.Errorf("unexpected content:\n%s", got)
	}

	noActor := buildCustomTelegramMessage(customTelegramMessage{Type: "issue_assigned", IssueTitle: "X", Status: "custom_state"})
	if !strings.Contains(noActor, "👤 Dir wurde „X“ zugewiesen\nStatus: custom_state") {
		t.Errorf("fallback wording wrong:\n%s", noActor)
	}
}

func TestCustomTelegramSnippetPicksMentioningParagraph(t *testing.T) {
	const me = "11111111-1111-1111-1111-111111111111"
	content := "Intro paragraph.\n\nSomething else.\n\nPlease review, [@Jens](mention://member/" + me + ")."
	if got := customTelegramSnippet(content, me); !strings.HasPrefix(got, "Please review") {
		t.Fatalf("snippet = %q", got)
	}
	if got := customTelegramSnippet("\n\nFirst.\n\nSecond.", me); got != "First." {
		t.Fatalf("fallback snippet = %q", got)
	}
	if got := customTelegramSnippet("A\n\n[@all](mention://all/all) heads up", me); !strings.Contains(got, "heads up") {
		t.Fatalf("@all snippet = %q", got)
	}
}

func TestCustomTelegramRenderTextTruncates(t *testing.T) {
	got := customTelegramRenderText(strings.Repeat("ä", 500), 400)
	if n := len([]rune(got)); n != 401 || !strings.HasSuffix(got, "…") {
		t.Fatalf("len = %d, suffix ok = %v", n, strings.HasSuffix(got, "…"))
	}
}

func TestCustomTelegramOnInboxNewFilters(t *testing.T) {
	s := &customTelegramNotify{queue: make(chan string, 10)}
	publish := func(recipientType, typ, id string) {
		item := inboxItemToResponse(db.InboxItem{
			ID:            util.MustParseUUID(id),
			RecipientType: recipientType,
			Type:          typ,
		})
		s.onInboxNew(events.Event{Type: protocol.EventInboxNew, Payload: map[string]any{"item": item}})
	}
	publish("member", "mentioned", "00000000-0000-0000-0000-000000000001")
	publish("member", "issue_assigned", "00000000-0000-0000-0000-000000000002")
	publish("member", "new_comment", "00000000-0000-0000-0000-000000000003")
	publish("member", "status_changed", "00000000-0000-0000-0000-000000000004")
	publish("agent", "mentioned", "00000000-0000-0000-0000-000000000005")
	s.onInboxNew(events.Event{Payload: "garbage"})

	close(s.queue)
	var got []string
	for id := range s.queue {
		got = append(got, id)
	}
	want := []string{"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("queued = %v, want %v", got, want)
	}
}

func TestCustomTelegramAPIScrubsToken(t *testing.T) {
	a := newCustomTelegramBotAPI("http://127.0.0.1:1", "secret-token-123")
	err := a.call(context.Background(), "getMe", struct{}{}, nil)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "secret-token-123") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func newTestCustomTelegram(t *testing.T, f *fakeTelegram) *customTelegramNotify {
	s := &customTelegramNotify{
		configured:  true,
		pool:        testPool,
		queries:     db.New(testPool),
		api:         newCustomTelegramBotAPI(f.srv.URL, "test-token"),
		appURL:      "https://multica.example",
		now:         time.Now,
		botUsername: "multica_test_bot",
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM custom_telegram_link WHERE user_id = $1`, testUserID)
		_, _ = testPool.Exec(ctx, `DELETE FROM custom_telegram_link_token WHERE user_id = $1`, testUserID)
	})
	return s
}

func TestCustomTelegramLinkAndDeliver(t *testing.T) {
	if testPool == nil {
		t.Skip("no database")
	}
	ctx := context.Background()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	f := newFakeTelegram(t)
	s := newTestCustomTelegram(t, f)
	userID := util.MustParseUUID(testUserID)

	// Link via deep-link token and /start.
	url, _, err := s.mintLinkToken(ctx, userID)
	if err != nil {
		t.Fatalf("mintLinkToken: %v", err)
	}
	prefix := "https://t.me/multica_test_bot?start="
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("link = %q", url)
	}
	token := strings.TrimPrefix(url, prefix)
	start := func(text string, chatID int64) {
		m := &customTelegramBotMessage{Text: text, From: &customTelegramBotUser{ID: chatID, Username: "jens_tg"}}
		m.Chat.ID = chatID
		m.Chat.Type = "private"
		s.handleMessage(ctx, m)
	}
	start("/start "+token, 4242)
	link, ok, err := s.loadLink(ctx, userID)
	if err != nil || !ok {
		t.Fatalf("link not stored: ok=%v err=%v", ok, err)
	}
	if link.ChatID != 4242 || link.TelegramUsername != "jens_tg" || !link.NotifyMentions || !link.NotifyAssignments {
		t.Fatalf("link = %+v", link)
	}
	if msgs := f.messages(); len(msgs) != 1 || !strings.Contains(msgs[0]["text"].(string), "Verbunden") {
		t.Fatalf("confirmation not sent: %v", msgs)
	}

	// The token is single-use.
	start("/start "+token, 9999)
	if l, _, _ := s.loadLink(ctx, userID); l.ChatID != 4242 {
		t.Fatalf("reused token relinked chat to %d", l.ChatID)
	}

	// A mention in a comment is delivered with workspace, identifier and snippet.
	issueID := fx.Issue(t, "Telegram test issue")
	commentID := fx.Comment(t, issueID, "Kannst du das prüfen, [@Me](mention://member/"+testUserID+")?")
	mentionItem := fx.Insert(t, "inbox_item", testutil.Cols{
		"workspace_id":   testWorkspaceID,
		"recipient_type": "member",
		"recipient_id":   testUserID,
		"type":           "mentioned",
		"issue_id":       issueID,
		"title":          "Telegram test issue",
		"actor_type":     "member",
		"actor_id":       testUserID,
		"details":        `{"comment_id":"` + commentID + `"}`,
	})
	before := len(f.messages())
	if err := s.deliver(ctx, mentionItem); err != nil {
		t.Fatalf("deliver mention: %v", err)
	}
	msgs := f.messages()
	if len(msgs) != before+1 {
		t.Fatalf("mention not sent")
	}
	text := msgs[len(msgs)-1]["text"].(string)
	if msgs[len(msgs)-1]["chat_id"].(float64) != 4242 {
		t.Fatalf("sent to chat %v", msgs[len(msgs)-1]["chat_id"])
	}
	for _, want := range []string{"🏢 <b>", "erwähnt", "Kannst du das prüfen, @Me?", "https://multica.example/"} {
		if !strings.Contains(text, want) {
			t.Errorf("mention message missing %q:\n%s", want, text)
		}
	}

	// Switching mentions off stops them; assignments still go through.
	if _, err := testPool.Exec(ctx, `UPDATE custom_telegram_link SET notify_mentions = false WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	before = len(f.messages())
	if err := s.deliver(ctx, mentionItem); err != nil {
		t.Fatalf("deliver muted mention: %v", err)
	}
	if len(f.messages()) != before {
		t.Fatalf("mention sent although switched off")
	}
	assignItem := fx.Insert(t, "inbox_item", testutil.Cols{
		"workspace_id":   testWorkspaceID,
		"recipient_type": "member",
		"recipient_id":   testUserID,
		"type":           "issue_assigned",
		"severity":       "action_required",
		"issue_id":       issueID,
		"title":          "Telegram test issue",
		"actor_type":     "member",
		"actor_id":       testUserID,
	})
	if err := s.deliver(ctx, assignItem); err != nil {
		t.Fatalf("deliver assignment: %v", err)
	}
	if msgs := f.messages(); len(msgs) != before+1 || !strings.Contains(msgs[len(msgs)-1]["text"].(string), "zugewiesen") {
		t.Fatalf("assignment not sent: %v", msgs)
	}

	// A blocked bot unlinks the chat.
	f.mu.Lock()
	f.sendCode = http.StatusForbidden
	f.mu.Unlock()
	if err := s.deliver(ctx, assignItem); err != nil {
		t.Fatalf("deliver to blocked chat: %v", err)
	}
	if _, ok, _ := s.loadLink(ctx, userID); ok {
		t.Fatal("link kept although the bot was blocked")
	}
}

func TestCustomTelegramStop(t *testing.T) {
	if testPool == nil {
		t.Skip("no database")
	}
	ctx := context.Background()
	f := newFakeTelegram(t)
	s := newTestCustomTelegram(t, f)
	if _, err := testPool.Exec(ctx,
		`INSERT INTO custom_telegram_link (user_id, chat_id) VALUES ($1, 777)`, testUserID); err != nil {
		t.Fatal(err)
	}
	m := &customTelegramBotMessage{Text: "/stop"}
	m.Chat.ID = 777
	m.Chat.Type = "private"
	s.handleMessage(ctx, m)
	if _, ok, _ := s.loadLink(ctx, util.MustParseUUID(testUserID)); ok {
		t.Fatal("/stop did not unlink")
	}
}

func TestCustomTelegramExpiredToken(t *testing.T) {
	if testPool == nil {
		t.Skip("no database")
	}
	ctx := context.Background()
	f := newFakeTelegram(t)
	s := newTestCustomTelegram(t, f)
	s.now = func() time.Time { return time.Now().Add(-time.Hour) } // minted an hour ago → expired
	url, _, err := s.mintLinkToken(ctx, util.MustParseUUID(testUserID))
	if err != nil {
		t.Fatal(err)
	}
	s.now = time.Now
	token := url[strings.LastIndex(url, "=")+1:]
	if _, err := s.redeemLinkToken(ctx, token, 1, ""); err != errCustomTelegramTokenInvalid {
		t.Fatalf("redeem expired token: err = %v", err)
	}
}
