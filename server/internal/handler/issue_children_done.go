package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Handoff policies stored in issue.on_children_done (migration 235, MUL-5472).
//
// Before this, the child-done path had exactly one reaction — wake the
// parent's assignee — which costs a full agent run even when the parent has
// nothing left to coordinate. On a tree whose children already reported their
// own results, that run reads the children and restates them, then flips the
// parent to in_review: a whole turn spent on bookkeeping.
//
// The policies below are what the parent can ask for instead. `auto` is the
// default and the point of the design: the server infers the answer from the
// tree's shape so nobody has to configure anything, and the explicit values
// exist only as an override for when the inference is wrong.
const (
	onChildrenDoneAuto   = "auto"
	onChildrenDoneWake   = "wake"
	onChildrenDoneNotify = "notify"
	onChildrenDoneClose  = "close"
	onChildrenDoneOff    = "off"
)

// inboxTypeChildrenDone is the actionable inbox row raised by the notify
// policy. Severity is action_required: it is a question addressed to a person,
// not a status broadcast.
const inboxTypeChildrenDone = "children_done"

// validOnChildrenDone mirrors the CHECK constraint on issue.on_children_done
// so write handlers return a clean 400 instead of a database error.
var validOnChildrenDone = []string{
	onChildrenDoneAuto,
	onChildrenDoneWake,
	onChildrenDoneNotify,
	onChildrenDoneClose,
	onChildrenDoneOff,
}

func isValidOnChildrenDone(v string) bool {
	for _, s := range validOnChildrenDone {
		if s == v {
			return true
		}
	}
	return false
}

// isDeliveredChildStatus reports whether a child has handed its work over —
// either terminal (done/cancelled) or sitting in in_review.
//
// This is deliberately wider than isTerminalChildStatus. Every agent's
// workflow ENDS at in_review ("When done, run `multica issue status <id>
// in_review`"), so a fully agent-run subtree never reaches an all-terminal
// state on its own; it waits for a human to walk the list clicking done. The
// terminal barrier stays the gate for anything that PROMOTES work (see
// resolveOnChildrenDone), but the cheap "are we finished?" question can and
// should fire as soon as the work is delivered.
func isDeliveredChildStatus(status string) bool {
	return isTerminalChildStatus(status) || status == "in_review"
}

// childrenAllDelivered reports whether every child has handed its work over.
// An empty set is never "delivered" — a parent with no sub-issues has nothing
// to roll up.
func childrenAllDelivered(children []db.Issue) bool {
	if len(children) == 0 {
		return false
	}
	for _, c := range children {
		if !isDeliveredChildStatus(c.Status) {
			return false
		}
	}
	return true
}

// resolveOnChildrenDone turns the parent's stored policy into the concrete
// action to take. An explicit value wins; `auto` infers.
//
// The inference: an agent or squad assignee means somebody deliberately put an
// agent in charge of this parent, so keep waking it — that is today's behavior
// and the autonomous serial relay depends on it (a squad leader owns advancing
// its parent; see the Squad Operating Protocol and MUL-3969 / MUL-4063).
// Everything else — human-owned and unassigned parents — gets the receipt and
// the question instead. Those parents are woken by nothing at all today: the
// wake path skips member assignees outright (MUL-2538), so 36% of the parents
// in a real workspace currently produce no signal whatsoever when their
// sub-issues finish. That silence is what pushed users to reassign parents to
// themselves as a mute switch.
//
// Deliberately NOT inferred here: downgrading an agent-owned parent to notify
// by default. That is the change that would fix "the parent agent just
// restates the children", but it trades away autonomy the serial relay was
// built for, so it stays a one-click opt-in (`notify` / `close`) rather than a
// silent default flip.
func resolveOnChildrenDone(parent db.Issue, children []db.Issue) string {
	if parent.OnChildrenDone != onChildrenDoneAuto && isValidOnChildrenDone(parent.OnChildrenDone) {
		return parent.OnChildrenDone
	}
	if parent.AssigneeType.Valid &&
		(parent.AssigneeType.String == "agent" || parent.AssigneeType.String == "squad") {
		return onChildrenDoneWake
	}
	return onChildrenDoneNotify
}

// rollupStatusForParent returns the status a parent may roll up to, given its
// children's current state.
//
// This is the invariant that makes automatic close safe without a single extra
// setting:
//
//	A parent never rolls up past its weakest child.
//
// Every child accepted (done/cancelled) -> the parent may reach done. Any
// child merely delivered (in_review, not yet accepted) -> the parent stops at
// in_review too. So "close it out" can never mark a parent finished on top of
// work nobody has looked at, and the user does not have to reason about that
// case — the rule holds by construction.
func rollupStatusForParent(children []db.Issue) string {
	for _, c := range children {
		if !isTerminalChildStatus(c.Status) {
			return "in_review"
		}
	}
	return "done"
}

// childrenDoneReceipt renders the deterministic completion receipt posted on
// the parent. It lists what finished — identifier, title, status — and nothing
// else. No prose synthesis: either the list IS the summary (the container case
// this path serves), or the parent genuinely needs a real agent turn, which is
// what the wake policy and the [continue] action are for. A generated
// paragraph in between would only look like a summary.
func (h *Handler) childrenDoneReceipt(ctx context.Context, parent db.Issue, children []db.Issue) string {
	prefix := h.getIssuePrefix(ctx, parent.WorkspaceID)
	var b strings.Builder
	fmt.Fprintf(&b, "All %d sub-issues have been delivered.\n\n", len(children))
	for _, c := range children {
		identifier := prefix + "-" + strconv.Itoa(int(c.Number))
		// The issue mention renders as a chip carrying the title, so repeating
		// the title here would print it twice in the timeline.
		fmt.Fprintf(&b, "- [%s](mention://issue/%s) · `%s`\n",
			identifier, uuidToString(c.ID), c.Status)
	}
	b.WriteString("\nNo agent was started for this. Decide what happens to the parent from the Sub-issues section.")
	return b.String()
}

// postChildrenDoneReceipt posts the receipt comment and raises the actionable
// inbox row for whoever should decide. Best-effort throughout, exactly like
// the wake path: this rides alongside a committed status change and must never
// roll it back.
//
// Idempotency: an unarchived children_done row for this issue means the
// question is already open and the card is live. It recomputes its rollup
// target on every action, so a later child moving in_review -> done is
// reflected without re-posting anything.
func (h *Handler) postChildrenDoneReceipt(ctx context.Context, parent db.Issue, children []db.Issue) (db.Comment, bool) {
	open, err := h.Queries.HasOpenInboxItemForIssueAndType(ctx, db.HasOpenInboxItemForIssueAndTypeParams{
		WorkspaceID: parent.WorkspaceID,
		IssueID:     parent.ID,
		Type:        inboxTypeChildrenDone,
	})
	if err == nil && open {
		return db.Comment{}, false
	}

	comment, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     parent.ID,
		WorkspaceID: parent.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     h.childrenDoneReceipt(ctx, parent, children),
		Type:        "system",
		ParentID:    pgtype.UUID{Valid: false},
	})
	if err != nil {
		slog.Warn("children done: create receipt comment failed",
			"error", err, "parent_id", uuidToString(parent.ID))
		return db.Comment{}, false
	}

	h.publish(protocol.EventCommentCreated, uuidToString(parent.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         parent.Title,
		"issue_assignee_type": textToPtr(parent.AssigneeType),
		"issue_assignee_id":   uuidToPtr(parent.AssigneeID),
		"issue_status":        parent.Status,
	})

	h.raiseChildrenDoneInbox(ctx, parent, children)
	return comment, true
}

// childrenDoneDecider returns the member who should be asked. The parent's own
// assignee when that is a person, otherwise its creator when that is a person.
//
// A parent owned by an agent still gets the receipt comment, but no inbox row:
// there is no person the platform can name without guessing, and an inbox row
// addressed to the wrong human is worse than none. The agent's owner sees the
// receipt on the issue itself.
func childrenDoneDecider(parent db.Issue) (pgtype.UUID, bool) {
	if parent.AssigneeType.Valid && parent.AssigneeType.String == "member" && parent.AssigneeID.Valid {
		return parent.AssigneeID, true
	}
	if parent.CreatorType == "member" && parent.CreatorID.Valid {
		return parent.CreatorID, true
	}
	return pgtype.UUID{}, false
}

func (h *Handler) raiseChildrenDoneInbox(ctx context.Context, parent db.Issue, children []db.Issue) {
	recipient, ok := childrenDoneDecider(parent)
	if !ok {
		return
	}

	details, _ := json.Marshal(map[string]string{
		"child_count":   strconv.Itoa(len(children)),
		"rollup_status": rollupStatusForParent(children),
	})

	item, err := h.Queries.CreateInboxItem(ctx, db.CreateInboxItemParams{
		WorkspaceID:   parent.WorkspaceID,
		RecipientType: "member",
		RecipientID:   recipient,
		Type:          inboxTypeChildrenDone,
		Severity:      "action_required",
		IssueID:       parent.ID,
		Title:         parent.Title,
		Body: pgtype.Text{
			String: fmt.Sprintf("All %d sub-issues have been delivered. Continue, close it out, or leave it.", len(children)),
			Valid:  true,
		},
		ActorType: pgtype.Text{String: "system", Valid: true},
		ActorID:   pgtype.UUID{Valid: true},
		Details:   details,
	})
	if err != nil {
		slog.Warn("children done: create inbox item failed",
			"error", err, "parent_id", uuidToString(parent.ID))
		return
	}

	resp := inboxToResponse(item)
	resp.IssueStatus = &parent.Status
	h.publish(protocol.EventInboxNew, uuidToString(parent.WorkspaceID), "system", "", map[string]any{
		"item": resp,
	})
}

// applyChildrenDoneClose executes the close policy: receipt, then roll the
// parent up on its own, capped by rollupStatusForParent.
func (h *Handler) applyChildrenDoneClose(ctx context.Context, parent db.Issue, children []db.Issue) {
	h.postChildrenDoneReceipt(ctx, parent, children)
	h.rollUpParentStatus(ctx, parent, children, "children_done_close")
}

// rollUpParentStatus moves the parent to its rollup target and broadcasts the
// change. No-op when the parent is already at or past the target.
func (h *Handler) rollUpParentStatus(ctx context.Context, parent db.Issue, children []db.Issue, source string) (db.Issue, bool) {
	target := rollupStatusForParent(children)
	if parent.Status == target || isTerminalChildStatus(parent.Status) {
		return parent, false
	}

	updated, err := h.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		ID:          parent.ID,
		Status:      target,
		WorkspaceID: parent.WorkspaceID,
	})
	if err != nil {
		slog.Warn("children done: roll up parent status failed",
			"error", err, "parent_id", uuidToString(parent.ID))
		return parent, false
	}

	prefix := h.getIssuePrefix(ctx, parent.WorkspaceID)
	h.publish(protocol.EventIssueUpdated, uuidToString(parent.WorkspaceID), "system", "", map[string]any{
		"issue":          issueToResponse(updated, prefix),
		"status_changed": true,
		"prev_status":    parent.Status,
		"creator_type":   parent.CreatorType,
		"creator_id":     uuidToString(parent.CreatorID),
		"source":         source,
	})
	return updated, true
}

// ChildrenDoneActionRequest is the body of the decision the owner makes on the
// receipt card.
type ChildrenDoneActionRequest struct {
	// Action is one of continue | close | dismiss.
	Action string `json:"action"`
}

// ChildrenDoneAction answers the "all sub-issues are delivered — now what?"
// question raised by the notify policy.
//
// This endpoint is the reason the design does not need up-front configuration.
// The click IS the decision: nothing has to be set before the tree runs, and
// the same click can be promoted into a standing policy afterwards by setting
// on_children_done on the parent.
//
//	continue — the parent does have its own work left: wake its assignee, the
//	           same dispatch the wake policy would have made.
//	close    — accept the children's work: roll the parent up, capped by the
//	           weakest-child invariant.
//	dismiss  — leave the parent alone.
//
// Every action resolves the open card.
func (h *Handler) ChildrenDoneAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	parent, ok := h.loadIssueForUser(w, r, id)
	if !ok {
		return
	}

	var req ChildrenDoneActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.Action {
	case "continue", "close", "dismiss":
	default:
		writeError(w, http.StatusBadRequest, "action must be one of: continue, close, dismiss")
		return
	}

	ctx := r.Context()
	children, err := h.Queries.ListChildIssues(ctx, parent.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load sub-issues")
		return
	}

	result := parent
	switch req.Action {
	case "continue":
		h.continueParentAfterChildrenDone(ctx, parent, children)
	case "close":
		result, _ = h.rollUpParentStatus(ctx, parent, children, "children_done_action")
	}

	h.resolveChildrenDoneInbox(ctx, parent)

	prefix := h.getIssuePrefix(ctx, parent.WorkspaceID)
	writeJSON(w, http.StatusOK, map[string]any{
		"issue":  issueToResponse(result, prefix),
		"action": req.Action,
	})
}

// continueParentAfterChildrenDone records the decision in the timeline and
// wakes the parent's assignee off that comment — the same dispatch the wake
// policy makes, so the woken agent lands in an identical shape.
func (h *Handler) continueParentAfterChildrenDone(ctx context.Context, parent db.Issue, children []db.Issue) {
	mentionPrefix := h.buildParentAssigneeMention(ctx, parent)
	content := fmt.Sprintf(
		"%sAll %d sub-issues are delivered and the owner asked to continue the parent. Synthesize the children's results and move it forward, or — if nothing remains — run `multica issue status %s in_review` to mark the parent ready for review.",
		mentionPrefix, len(children), uuidToString(parent.ID),
	)

	comment, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     parent.ID,
		WorkspaceID: parent.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     content,
		Type:        "system",
		ParentID:    pgtype.UUID{Valid: false},
	})
	if err != nil {
		slog.Warn("children done: create continue comment failed",
			"error", err, "parent_id", uuidToString(parent.ID))
		return
	}

	h.publish(protocol.EventCommentCreated, uuidToString(parent.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         parent.Title,
		"issue_assignee_type": textToPtr(parent.AssigneeType),
		"issue_assignee_id":   uuidToPtr(parent.AssigneeID),
		"issue_status":        parent.Status,
	})

	h.dispatchParentAssigneeTrigger(ctx, parent, comment)
}

// resolveChildrenDoneInbox archives the open card once its question has been
// answered.
func (h *Handler) resolveChildrenDoneInbox(ctx context.Context, parent db.Issue) {
	recipient, ok := childrenDoneDecider(parent)
	if !ok {
		return
	}
	if _, err := h.Queries.ArchiveInboxByIssue(ctx, db.ArchiveInboxByIssueParams{
		WorkspaceID:   parent.WorkspaceID,
		RecipientType: "member",
		RecipientID:   recipient,
		IssueID:       parent.ID,
	}); err != nil {
		slog.Warn("children done: archive inbox failed",
			"error", err, "parent_id", uuidToString(parent.ID))
		return
	}
	h.publish(protocol.EventInboxArchived, uuidToString(parent.WorkspaceID), "system", "", map[string]any{
		"issue_id": uuidToString(parent.ID),
	})
}
