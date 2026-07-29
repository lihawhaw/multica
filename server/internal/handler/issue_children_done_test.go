package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// --- unit: the invariant -----------------------------------------------------

func issueWithStatus(status string) db.Issue {
	return db.Issue{Status: status}
}

// A parent must never roll up past its weakest child. This is the rule that
// lets "close it out" exist without a second safety setting, so it is pinned
// directly rather than only through the endpoint.
func TestRollupStatusNeverPassesWeakestChild(t *testing.T) {
	cases := []struct {
		name     string
		children []db.Issue
		want     string
	}{
		{
			name:     "every child accepted reaches done",
			children: []db.Issue{issueWithStatus("done"), issueWithStatus("done")},
			want:     "done",
		},
		{
			name:     "cancelled counts as accepted",
			children: []db.Issue{issueWithStatus("done"), issueWithStatus("cancelled")},
			want:     "done",
		},
		{
			name:     "one merely delivered child caps the parent at in_review",
			children: []db.Issue{issueWithStatus("done"), issueWithStatus("in_review")},
			want:     "in_review",
		},
		{
			name:     "all delivered but none accepted caps at in_review",
			children: []db.Issue{issueWithStatus("in_review"), issueWithStatus("in_review")},
			want:     "in_review",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rollupStatusForParent(tc.children); got != tc.want {
				t.Fatalf("rollupStatusForParent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveOnChildrenDone(t *testing.T) {
	agentParent := db.Issue{
		AssigneeType:   pgtype.Text{String: "agent", Valid: true},
		OnChildrenDone: onChildrenDoneAuto,
	}
	if got := resolveOnChildrenDone(agentParent, nil); got != onChildrenDoneWake {
		t.Errorf("agent-owned parent should keep waking, got %q", got)
	}

	squadParent := db.Issue{
		AssigneeType:   pgtype.Text{String: "squad", Valid: true},
		OnChildrenDone: onChildrenDoneAuto,
	}
	if got := resolveOnChildrenDone(squadParent, nil); got != onChildrenDoneWake {
		t.Errorf("squad-owned parent should keep waking, got %q", got)
	}

	memberParent := db.Issue{
		AssigneeType:   pgtype.Text{String: "member", Valid: true},
		OnChildrenDone: onChildrenDoneAuto,
	}
	if got := resolveOnChildrenDone(memberParent, nil); got != onChildrenDoneNotify {
		t.Errorf("human-owned parent should be asked, got %q", got)
	}

	// An explicit policy always wins over the inference.
	explicit := db.Issue{
		AssigneeType:   pgtype.Text{String: "agent", Valid: true},
		OnChildrenDone: onChildrenDoneClose,
	}
	if got := resolveOnChildrenDone(explicit, nil); got != onChildrenDoneClose {
		t.Errorf("explicit policy should win, got %q", got)
	}
}

// --- integration -------------------------------------------------------------

func setOnChildrenDone(t *testing.T, issueID, policy string) {
	t.Helper()
	w := httptest.NewRecorder()
	req := withURLParam(
		newRequest("PUT", "/api/issues/"+issueID, map[string]any{"on_children_done": policy}),
		"id", issueID)
	testHandler.UpdateIssue(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("set on_children_done=%s: expected 200, got %d: %s", policy, w.Code, w.Body.String())
	}
	var got IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	if got.OnChildrenDone != policy {
		t.Fatalf("API round-trip: on_children_done = %q, want %q", got.OnChildrenDone, policy)
	}
}

func childrenDoneAction(t *testing.T, issueID, action string) IssueResponse {
	t.Helper()
	w := httptest.NewRecorder()
	req := withURLParam(
		newRequest("POST", "/api/issues/"+issueID+"/children-done-action", map[string]any{"action": action}),
		"id", issueID)
	testHandler.ChildrenDoneAction(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("children-done-action %s: expected 200, got %d: %s", action, w.Code, w.Body.String())
	}
	var resp struct {
		Issue IssueResponse `json:"issue"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode action response: %v", err)
	}
	return resp.Issue
}

func issueStatusOf(t *testing.T, issueID string) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("read issue status: %v", err)
	}
	return status
}

// A child that only reaches in_review still counts as delivered, so the
// question is raised. This is the transition that used to strand a fully
// agent-run subtree: agents finish at in_review, never at done.
func TestChildrenDeliveredRaisesReceiptAtInReview(t *testing.T) {
	fx := newChildDoneFixture(t, "in_progress")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE issue_id = $1`, fx.parent.ID)
	})

	updateChildStatus(t, fx.child.ID, "in_review")

	if got := countSystemCommentsOn(t, fx.parent.ID); got != 1 {
		t.Fatalf("expected one receipt when the child reached in_review, got %d", got)
	}
	content, _, _, _ := systemCommentOn(t, fx.parent.ID)
	if !strings.Contains(content, "No agent was started") {
		t.Errorf("receipt should say no agent ran, got: %s", content)
	}
	// The parent's own status is untouched — the receipt asks, it does not act.
	if got := issueStatusOf(t, fx.parent.ID); got != "in_progress" {
		t.Errorf("notify must not move the parent, status = %q", got)
	}
}

// Closing out from the card respects the invariant: a child that is only
// delivered caps the parent at in_review, even though the user asked to
// "close it out".
func TestChildrenDoneActionCloseIsCappedByWeakestChild(t *testing.T) {
	fx := newChildDoneFixture(t, "in_progress")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE issue_id = $1`, fx.parent.ID)
	})

	updateChildStatus(t, fx.child.ID, "in_review")
	updated := childrenDoneAction(t, fx.parent.ID, "close")
	if updated.Status != "in_review" {
		t.Fatalf("child only delivered: parent should stop at in_review, got %q", updated.Status)
	}

	// Accept the child, ask again — now the parent may finish.
	updateChildStatus(t, fx.child.ID, "done")
	updated = childrenDoneAction(t, fx.parent.ID, "close")
	if updated.Status != "done" {
		t.Fatalf("child accepted: parent should reach done, got %q", updated.Status)
	}
}

// Answering the question archives the card so the same question is not asked
// twice.
func TestChildrenDoneActionResolvesTheCard(t *testing.T) {
	fx := newChildDoneFixture(t, "in_progress")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE issue_id = $1`, fx.parent.ID)
	})

	var userID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT user_id FROM member WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID,
	).Scan(&userID); err != nil {
		t.Fatalf("locate workspace member: %v", err)
	}
	setIssueAssigneeDirect(t, fx.parent.ID, "member", userID)

	updateChildStatus(t, fx.child.ID, "done")
	if got := countInboxItems(t, userID, fx.parent.ID); got != 1 {
		t.Fatalf("expected one open card, got %d", got)
	}

	childrenDoneAction(t, fx.parent.ID, "dismiss")

	var open int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND archived = false`, fx.parent.ID,
	).Scan(&open); err != nil {
		t.Fatalf("count open inbox rows: %v", err)
	}
	if open != 0 {
		t.Fatalf("dismiss should resolve the card, %d still open", open)
	}
	// Dismiss leaves the parent alone.
	if got := issueStatusOf(t, fx.parent.ID); got != "in_progress" {
		t.Errorf("dismiss must not move the parent, status = %q", got)
	}
}

// The close policy does the rollup on its own, with no card and no agent.
func TestOnChildrenDoneClosePolicyRollsUpAutomatically(t *testing.T) {
	fx := newChildDoneFixture(t, "in_progress")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE issue_id = $1`, fx.parent.ID)
	})
	setOnChildrenDone(t, fx.parent.ID, onChildrenDoneClose)

	updateChildStatus(t, fx.child.ID, "done")

	if got := issueStatusOf(t, fx.parent.ID); got != "done" {
		t.Fatalf("close policy should roll the parent up to done, got %q", got)
	}
}

// The off policy suppresses the whole path — no comment, no card, no status
// change.
func TestOnChildrenDoneOffPolicyIsSilent(t *testing.T) {
	fx := newChildDoneFixture(t, "in_progress")
	setOnChildrenDone(t, fx.parent.ID, onChildrenDoneOff)

	updateChildStatus(t, fx.child.ID, "done")

	if got := countSystemCommentsOn(t, fx.parent.ID); got != 0 {
		t.Errorf("off policy should post nothing, got %d comments", got)
	}
	if got := issueStatusOf(t, fx.parent.ID); got != "in_progress" {
		t.Errorf("off policy should not move the parent, status = %q", got)
	}
}

func TestChildrenDoneActionRejectsUnknownAction(t *testing.T) {
	fx := newChildDoneFixture(t, "in_progress")
	w := httptest.NewRecorder()
	req := withURLParam(
		newRequest("POST", "/api/issues/"+fx.parent.ID+"/children-done-action",
			map[string]any{"action": "delete-everything"}),
		"id", fx.parent.ID)
	testHandler.ChildrenDoneAction(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
