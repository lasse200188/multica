package main

// Custom addition (fork lasse200188/multica, see CUSTOM.md): HTTP routes for
// the Telegram section in Settings → Profile. User-scoped (no workspace): the
// link belongs to the person and covers all workspaces.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

func registerCustomTelegramRoutes(r chi.Router) {
	r.Get("/api/me/custom/telegram", customTelegramGetStatus)
	r.Patch("/api/me/custom/telegram", customTelegramUpdateSettings)
	r.Delete("/api/me/custom/telegram", customTelegramUnlink)
	r.Post("/api/me/custom/telegram/link", customTelegramCreateLink)
	r.Post("/api/me/custom/telegram/test", customTelegramSendTest)
}

type customTelegramStatusResponse struct {
	Configured        bool    `json:"configured"`
	BotUsername       string  `json:"bot_username"`
	Connected         bool    `json:"connected"`
	TelegramUsername  string  `json:"telegram_username"`
	NotifyMentions    bool    `json:"notify_mentions"`
	NotifyAssignments bool    `json:"notify_assignments"`
	LinkedAt          *string `json:"linked_at"`
}

func customTelegramJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func customTelegramError(w http.ResponseWriter, status int, msg string) {
	customTelegramJSON(w, status, map[string]string{"error": msg})
}

// customTelegramUser returns the signed-in user's id, writing an error
// response when there is none.
func customTelegramUser(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := util.ParseUUID(r.Header.Get("X-User-ID"))
	if err != nil || !id.Valid {
		customTelegramError(w, http.StatusUnauthorized, "user not authenticated")
		return pgtype.UUID{}, false
	}
	return id, true
}

// customTelegramReady returns the running feature or answers 503.
func customTelegramReady(w http.ResponseWriter) (*customTelegramNotify, bool) {
	s := customTelegram
	if s == nil || !s.configured {
		customTelegramError(w, http.StatusServiceUnavailable, "telegram notifications are not configured on this server")
		return nil, false
	}
	return s, true
}

func customTelegramStatus(r *http.Request, s *customTelegramNotify, userID pgtype.UUID) (customTelegramStatusResponse, error) {
	resp := customTelegramStatusResponse{NotifyMentions: true, NotifyAssignments: true}
	if s == nil || !s.configured {
		return resp, nil
	}
	resp.Configured = true
	resp.BotUsername = s.username()
	link, ok, err := s.loadLink(r.Context(), userID)
	if err != nil {
		return resp, err
	}
	if ok {
		resp.Connected = true
		resp.TelegramUsername = link.TelegramUsername
		resp.NotifyMentions = link.NotifyMentions
		resp.NotifyAssignments = link.NotifyAssignments
		at := link.LinkedAt.UTC().Format(time.RFC3339)
		resp.LinkedAt = &at
	}
	return resp, nil
}

func customTelegramGetStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := customTelegramUser(w, r)
	if !ok {
		return
	}
	resp, err := customTelegramStatus(r, customTelegram, userID)
	if err != nil {
		customTelegramError(w, http.StatusInternalServerError, "failed to load telegram status")
		return
	}
	customTelegramJSON(w, http.StatusOK, resp)
}

func customTelegramUpdateSettings(w http.ResponseWriter, r *http.Request) {
	userID, ok := customTelegramUser(w, r)
	if !ok {
		return
	}
	s, ok := customTelegramReady(w)
	if !ok {
		return
	}
	var body struct {
		NotifyMentions    *bool `json:"notify_mentions"`
		NotifyAssignments *bool `json:"notify_assignments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		customTelegramError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tag, err := s.pool.Exec(r.Context(), `
		UPDATE custom_telegram_link
		SET notify_mentions = COALESCE($2, notify_mentions),
		    notify_assignments = COALESCE($3, notify_assignments)
		WHERE user_id = $1`, userID, body.NotifyMentions, body.NotifyAssignments)
	if err != nil {
		customTelegramError(w, http.StatusInternalServerError, "failed to update telegram settings")
		return
	}
	if tag.RowsAffected() == 0 {
		customTelegramError(w, http.StatusNotFound, "telegram is not connected")
		return
	}
	resp, err := customTelegramStatus(r, s, userID)
	if err != nil {
		customTelegramError(w, http.StatusInternalServerError, "failed to load telegram status")
		return
	}
	customTelegramJSON(w, http.StatusOK, resp)
}

func customTelegramUnlink(w http.ResponseWriter, r *http.Request) {
	userID, ok := customTelegramUser(w, r)
	if !ok {
		return
	}
	s, ok := customTelegramReady(w)
	if !ok {
		return
	}
	link, found, err := s.loadLink(r.Context(), userID)
	if err != nil {
		customTelegramError(w, http.StatusInternalServerError, "failed to load telegram status")
		return
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM custom_telegram_link WHERE user_id = $1`, userID); err != nil {
		customTelegramError(w, http.StatusInternalServerError, "failed to disconnect telegram")
		return
	}
	if found {
		// Best effort; the link is gone either way.
		_ = s.api.sendMessage(r.Context(), link.ChatID, "Verbindung zu Multica getrennt. Du bekommst hier keine Nachrichten mehr.")
	}
	w.WriteHeader(http.StatusNoContent)
}

func customTelegramCreateLink(w http.ResponseWriter, r *http.Request) {
	userID, ok := customTelegramUser(w, r)
	if !ok {
		return
	}
	s, ok := customTelegramReady(w)
	if !ok {
		return
	}
	url, expires, err := s.mintLinkToken(r.Context(), userID)
	if err != nil {
		customTelegramError(w, http.StatusServiceUnavailable, "telegram bot is not ready, try again in a moment")
		return
	}
	customTelegramJSON(w, http.StatusOK, map[string]string{
		"url":        url,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

func customTelegramSendTest(w http.ResponseWriter, r *http.Request) {
	userID, ok := customTelegramUser(w, r)
	if !ok {
		return
	}
	s, ok := customTelegramReady(w)
	if !ok {
		return
	}
	link, found, err := s.loadLink(r.Context(), userID)
	if err != nil {
		customTelegramError(w, http.StatusInternalServerError, "failed to load telegram status")
		return
	}
	if !found {
		customTelegramError(w, http.StatusNotFound, "telegram is not connected")
		return
	}
	err = s.api.sendMessage(r.Context(), link.ChatID,
		"🔔 Testnachricht von Multica.\nHier kommen deine Erwähnungen und Zuweisungen an.")
	var apiErr *customTelegramAPIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden {
		_, _ = s.pool.Exec(r.Context(), `DELETE FROM custom_telegram_link WHERE user_id = $1`, userID)
		customTelegramError(w, http.StatusGone, "the chat is no longer reachable (bot blocked?), please connect again")
		return
	}
	if err != nil {
		customTelegramError(w, http.StatusBadGateway, "sending the test message failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
