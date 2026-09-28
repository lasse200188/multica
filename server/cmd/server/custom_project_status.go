package main

// Custom addition (fork lasse200188/multica, see CUSTOM.md): a project's status
// follows its issues.
//
//   - planned / in_progress → completed   when every issue is done or cancelled
//     and at least one is done
//   - completed → in_progress             when any issue is open again
//   - planned → in_progress               when any issue has started
//   - paused / cancelled                  are never touched (manual brake)
//
// Issue events only carry the workspace we need to look at: deletes and
// project moves do not say which project the issue left, so every project of
// the workspace is recomputed. That is a handful of grouped counts over an
// indexed column. A periodic sweep catches writes that publish no event
// (e.g. the runtime sweeper resetting issues).
//
// MULTICA_CUSTOM_PROJECT_AUTO_STATUS=off disables the feature.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	customProjectAutoStatusEnv   = "MULTICA_CUSTOM_PROJECT_AUTO_STATUS"
	customProjectStatusDebounce  = 2 * time.Second
	customProjectStatusSweep     = 10 * time.Minute
	customProjectStatusFirstScan = 30 * time.Second
)

type customProjectIssueCounts struct {
	Total    int64 // all issues in the project, sub-issues included
	Terminal int64 // category done or closed
	Done     int64 // category done
	Started  int64 // category started
}

// deriveCustomProjectStatus returns the status a project should move to, and
// whether it should move at all.
func deriveCustomProjectStatus(current string, c customProjectIssueCounts) (string, bool) {
	switch current {
	case "planned", "in_progress":
		if c.Total > 0 && c.Terminal == c.Total && c.Done > 0 {
			return "completed", true
		}
		if current == "planned" && c.Started > 0 {
			return "in_progress", true
		}
	case "completed":
		if c.Terminal < c.Total {
			return "in_progress", true
		}
	}
	return current, false
}

type customProjectStatus struct {
	bus     *events.Bus
	pool    *pgxpool.Pool
	queries *db.Queries

	debounce time.Duration

	mu      sync.Mutex
	pending map[string]*time.Timer
}

func registerCustomProjectStatus(bus *events.Bus, pool *pgxpool.Pool, queries *db.Queries) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(customProjectAutoStatusEnv))) {
	case "off", "false", "0", "no":
		slog.Info("custom project auto status disabled", "env", customProjectAutoStatusEnv)
		return
	}
	s := &customProjectStatus{
		bus:      bus,
		pool:     pool,
		queries:  queries,
		debounce: customProjectStatusDebounce,
		pending:  make(map[string]*time.Timer),
	}
	for _, t := range []string{protocol.EventIssueCreated, protocol.EventIssueUpdated, protocol.EventIssueDeleted} {
		bus.Subscribe(t, s.onIssueEvent)
	}
	go s.sweepLoop()
	slog.Info("custom project auto status enabled")
}

// onIssueEvent runs on the publisher's goroutine, so it only schedules work.
func (s *customProjectStatus) onIssueEvent(e events.Event) {
	ws := e.WorkspaceID
	if ws == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.pending[ws]; ok {
		t.Reset(s.debounce)
		return
	}
	s.pending[ws] = time.AfterFunc(s.debounce, func() {
		s.mu.Lock()
		delete(s.pending, ws)
		s.mu.Unlock()
		s.reconcileLogged(ws)
	})
}

func (s *customProjectStatus) sweepLoop() {
	time.Sleep(customProjectStatusFirstScan)
	for {
		s.sweep()
		time.Sleep(customProjectStatusSweep)
	}
}

func (s *customProjectStatus) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT workspace_id FROM project WHERE status IN ('planned', 'in_progress', 'completed')`)
	if err != nil {
		slog.Warn("custom project auto status: sweep query failed", "error", err)
		return
	}
	var workspaces []string
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err == nil {
			workspaces = append(workspaces, util.UUIDToString(id))
		}
	}
	rows.Close()
	for _, ws := range workspaces {
		s.reconcileLogged(ws)
	}
}

func (s *customProjectStatus) reconcileLogged(ws string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.reconcileWorkspace(ctx, ws); err != nil {
		slog.Warn("custom project auto status: reconcile failed", "workspace_id", ws, "error", err)
	}
}

// reconcileWorkspace applies deriveCustomProjectStatus to every project of the
// workspace and returns how many projects changed.
func (s *customProjectStatus) reconcileWorkspace(ctx context.Context, ws string) (int, error) {
	wsUUID, err := util.ParseUUID(ws)
	if err != nil {
		return 0, err
	}
	projects, err := s.queries.ListProjects(ctx, db.ListProjectsParams{WorkspaceID: wsUUID})
	if err != nil {
		return 0, err
	}
	var ids []pgtype.UUID
	for _, p := range projects {
		switch p.Status {
		case "planned", "in_progress", "completed":
			ids = append(ids, p.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}

	terminal := s.statusKeys(ctx, wsUUID, []string{issuestatus.CategoryDone, issuestatus.CategoryClosed}, "done", "cancelled")
	done := s.statusKeys(ctx, wsUUID, []string{issuestatus.CategoryDone}, "done")
	started := s.statusKeys(ctx, wsUUID, []string{issuestatus.CategoryStarted}, "in_progress", "in_review", "blocked")

	counts := make(map[string]*customProjectIssueCounts, len(ids))
	for _, pass := range []struct {
		keys []string
		set  func(c *customProjectIssueCounts, n int64)
	}{
		{terminal, func(c *customProjectIssueCounts, n int64) { c.Terminal = n }},
		{done, func(c *customProjectIssueCounts, n int64) { c.Done = n }},
		{started, func(c *customProjectIssueCounts, n int64) { c.Started = n }},
	} {
		rows, err := s.queries.GetProjectIssueStats(ctx, db.GetProjectIssueStatsParams{
			WorkspaceID:        wsUUID,
			ProjectIds:         ids,
			TerminalStatusKeys: pass.keys,
		})
		if err != nil {
			return 0, err
		}
		for _, r := range rows {
			id := util.UUIDToString(r.ProjectID)
			c := counts[id]
			if c == nil {
				c = &customProjectIssueCounts{}
				counts[id] = c
			}
			c.Total = r.TotalCount
			pass.set(c, r.DoneCount)
		}
	}

	changed := 0
	for _, p := range projects {
		id := util.UUIDToString(p.ID)
		c := customProjectIssueCounts{}
		if counts[id] != nil {
			c = *counts[id]
		}
		next, move := deriveCustomProjectStatus(p.Status, c)
		if !move {
			continue
		}
		// Compare-and-set: a concurrent manual change wins.
		tag, err := s.pool.Exec(ctx,
			`UPDATE project SET status = $1, updated_at = now() WHERE id = $2 AND workspace_id = $3 AND status = $4`,
			next, p.ID, wsUUID, p.Status)
		if err != nil {
			return changed, err
		}
		if tag.RowsAffected() != 1 {
			continue
		}
		changed++
		slog.Info("project status auto-updated",
			"workspace_id", ws, "project_id", id, "project", p.Title,
			"from", p.Status, "to", next,
			"issues", c.Total, "terminal", c.Terminal, "done", c.Done, "started", c.Started)
		s.publishProject(ctx, wsUUID, p.ID, c)
	}
	return changed, nil
}

func (s *customProjectStatus) statusKeys(ctx context.Context, ws pgtype.UUID, categories []string, fallback ...string) []string {
	keys, err := issuestatus.ExpandCategories(ctx, s.queries, ws, categories)
	if err != nil || len(keys) == 0 {
		if err != nil {
			slog.Warn("custom project auto status: expand categories failed; using canonical keys",
				"workspace_id", util.UUIDToString(ws), "categories", categories, "error", err)
		}
		return fallback
	}
	return keys
}

// publishProject sends the same project:updated payload the project handler
// sends (handler.ProjectResponse keys): web and desktop only invalidate their
// cache, but mobile replaces its cached project with it.
func (s *customProjectStatus) publishProject(ctx context.Context, ws, projectID pgtype.UUID, c customProjectIssueCounts) {
	p, err := s.queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: ws})
	if err != nil {
		slog.Warn("custom project auto status: reload project failed", "project_id", util.UUIDToString(projectID), "error", err)
		return
	}
	var resources int64
	if rows, err := s.queries.GetProjectResourceCounts(ctx, []pgtype.UUID{projectID}); err == nil && len(rows) > 0 {
		resources = rows[0].ResourceCount
	}
	s.bus.Publish(events.Event{
		Type:        protocol.EventProjectUpdated,
		WorkspaceID: util.UUIDToString(ws),
		ActorType:   "system",
		Payload: map[string]any{"project": map[string]any{
			"id":             util.UUIDToString(p.ID),
			"workspace_id":   util.UUIDToString(p.WorkspaceID),
			"title":          p.Title,
			"description":    util.TextToPtr(p.Description),
			"icon":           util.TextToPtr(p.Icon),
			"status":         p.Status,
			"priority":       p.Priority,
			"lead_type":      util.TextToPtr(p.LeadType),
			"lead_id":        util.UUIDToPtr(p.LeadID),
			"start_date":     util.DateToPtr(p.StartDate),
			"due_date":       util.DateToPtr(p.DueDate),
			"created_at":     util.TimestampToString(p.CreatedAt),
			"updated_at":     util.TimestampToString(p.UpdatedAt),
			"issue_count":    c.Total,
			"done_count":     c.Terminal,
			"resource_count": resources,
		}},
	})
}
