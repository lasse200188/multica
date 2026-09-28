package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestDeriveCustomProjectStatus(t *testing.T) {
	c := func(total, terminal, done, started int64) customProjectIssueCounts {
		return customProjectIssueCounts{Total: total, Terminal: terminal, Done: done, Started: started}
	}
	cases := []struct {
		name    string
		current string
		counts  customProjectIssueCounts
		want    string
		move    bool
	}{
		{"planned without issues stays", "planned", c(0, 0, 0, 0), "planned", false},
		{"planned with only todo stays", "planned", c(3, 0, 0, 0), "planned", false},
		{"planned with a started issue starts", "planned", c(3, 0, 0, 1), "in_progress", true},
		{"planned all done completes", "planned", c(2, 2, 2, 0), "completed", true},
		{"in_progress all done completes", "in_progress", c(3, 3, 3, 0), "completed", true},
		{"in_progress done+cancelled completes", "in_progress", c(3, 3, 1, 0), "completed", true},
		{"in_progress only cancelled stays", "in_progress", c(2, 2, 0, 0), "in_progress", false},
		{"in_progress with open issue stays", "in_progress", c(3, 2, 2, 0), "in_progress", false},
		{"in_progress never falls back to planned", "in_progress", c(3, 0, 0, 0), "in_progress", false},
		{"in_progress without issues stays", "in_progress", c(0, 0, 0, 0), "in_progress", false},
		{"completed with reopened issue restarts", "completed", c(3, 2, 2, 0), "in_progress", true},
		{"completed all terminal stays", "completed", c(3, 3, 3, 0), "completed", false},
		{"completed without issues stays", "completed", c(0, 0, 0, 0), "completed", false},
		{"paused never touched", "paused", c(2, 2, 2, 0), "paused", false},
		{"cancelled never touched", "cancelled", c(2, 0, 0, 1), "cancelled", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, move := deriveCustomProjectStatus(tc.current, tc.counts)
			if got != tc.want || move != tc.move {
				t.Fatalf("derive(%q, %+v) = %q, %v; want %q, %v", tc.current, tc.counts, got, move, tc.want, tc.move)
			}
		})
	}
}

func TestCustomProjectStatusReconcile(t *testing.T) {
	if testPool == nil {
		t.Skip("no database")
	}
	ctx := context.Background()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	queries := db.New(testPool)
	bus := events.New()

	var mu sync.Mutex
	published := map[string]string{} // project id → status in payload
	bus.Subscribe(protocol.EventProjectUpdated, func(e events.Event) {
		p := e.Payload.(map[string]any)["project"].(map[string]any)
		mu.Lock()
		published[p["id"].(string)] = p["status"].(string)
		mu.Unlock()
		if e.ActorType != "system" {
			t.Errorf("actor type = %q, want system", e.ActorType)
		}
	})
	s := &customProjectStatus{bus: bus, pool: testPool, queries: queries}

	status := func(id string) string {
		t.Helper()
		p, err := queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
			ID: util.MustParseUUID(id), WorkspaceID: util.MustParseUUID(testWorkspaceID),
		})
		if err != nil {
			t.Fatalf("GetProjectInWorkspace: %v", err)
		}
		return p.Status
	}
	setIssue := func(id, st string) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `UPDATE issue SET status = $1 WHERE id = $2`, st, id); err != nil {
			t.Fatalf("update issue: %v", err)
		}
	}
	reconcile := func() {
		t.Helper()
		if _, err := s.reconcileWorkspace(ctx, testWorkspaceID); err != nil {
			t.Fatalf("reconcileWorkspace: %v", err)
		}
	}

	proj := fx.Project(t, "custom auto status")
	paused := fx.Project(t, "custom auto status paused", testutil.Cols{"status": "paused"})
	a := fx.Issue(t, "a", testutil.Cols{"project_id": proj})
	b := fx.Issue(t, "b", testutil.Cols{"project_id": proj})
	fx.Issue(t, "p", testutil.Cols{"project_id": paused, "status": "done"})

	reconcile()
	if got := status(proj); got != "planned" {
		t.Fatalf("only todo issues: status = %q, want planned", got)
	}

	setIssue(a, "in_progress")
	reconcile()
	if got := status(proj); got != "in_progress" {
		t.Fatalf("started issue: status = %q, want in_progress", got)
	}

	setIssue(a, "done")
	setIssue(b, "cancelled")
	reconcile()
	if got := status(proj); got != "completed" {
		t.Fatalf("done+cancelled: status = %q, want completed", got)
	}
	mu.Lock()
	if published[proj] != "completed" {
		t.Fatalf("published status = %q, want completed", published[proj])
	}
	mu.Unlock()

	fx.Issue(t, "c", testutil.Cols{"project_id": proj})
	reconcile()
	if got := status(proj); got != "in_progress" {
		t.Fatalf("new open issue: status = %q, want in_progress", got)
	}

	if got := status(paused); got != "paused" {
		t.Fatalf("paused project changed to %q", got)
	}
}

func TestCustomProjectStatusIssueEventTriggersReconcile(t *testing.T) {
	if testPool == nil {
		t.Skip("no database")
	}
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	bus := events.New()
	done := make(chan string, 4)
	bus.Subscribe(protocol.EventProjectUpdated, func(e events.Event) {
		done <- e.Payload.(map[string]any)["project"].(map[string]any)["status"].(string)
	})
	s := &customProjectStatus{bus: bus, pool: testPool, queries: db.New(testPool),
		debounce: 50 * time.Millisecond, pending: map[string]*time.Timer{}}
	bus.Subscribe(protocol.EventIssueUpdated, s.onIssueEvent)

	proj := fx.Project(t, "custom auto status event")
	fx.Issue(t, "e", testutil.Cols{"project_id": proj, "status": "done"})
	// Several events in a burst collapse into one reconcile.
	for i := 0; i < 3; i++ {
		bus.Publish(events.Event{Type: protocol.EventIssueUpdated, WorkspaceID: testWorkspaceID})
	}
	select {
	case st := <-done:
		if st != "completed" {
			t.Fatalf("status = %q, want completed", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no project:updated after issue event")
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(done); n != 0 {
		t.Fatalf("%d extra project:updated events, want none", n)
	}
}
