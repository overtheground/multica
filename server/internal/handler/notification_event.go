package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Shadow ingestion cannot select another member or installation. Task callers
// are bound to their actual originator and agent; human callers act as themselves.
func (h *Handler) notificationEventScope(w http.ResponseWriter, r *http.Request, agentID string) (pgtype.UUID, pgtype.UUID, pgtype.UUID, bool) {
	empty := pgtype.UUID{}
	if h.NotificationEvents == nil {
		writeError(w, 503, "notification event store unavailable")
		return empty, empty, empty, false
	}
	user, ok := requireUserID(w, r)
	if !ok {
		return empty, empty, empty, false
	}
	workspace := ctxWorkspaceID(r.Context())
	ws, ok := parseUUIDOrBadRequest(w, workspace, "workspace id")
	if !ok {
		return empty, empty, empty, false
	}
	if r.Header.Get("X-Actor-Source") == "task_token" {
		taskID, ok := parseUUIDOrBadRequest(w, r.Header.Get("X-Task-ID"), "task id")
		if !ok {
			return empty, empty, empty, false
		}
		task, err := h.Queries.GetAgentTask(r.Context(), taskID)
		if err != nil || !task.OriginatorUserID.Valid {
			writeError(w, 403, "notification requires a member originator")
			return empty, empty, empty, false
		}
		if agentID != "" && agentID != uuidToString(task.AgentID) {
			writeError(w, 403, "agent does not match task")
			return empty, empty, empty, false
		}
		if _, err = h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{WorkspaceID: ws, UserID: task.OriginatorUserID}); err != nil {
			writeError(w, 403, "originator is not a workspace member")
			return empty, empty, empty, false
		}
		a, err := h.Queries.GetAgent(r.Context(), task.AgentID)
		if err != nil || a.WorkspaceID != ws {
			writeError(w, 403, "agent workspace mismatch")
			return empty, empty, empty, false
		}
		return ws, task.OriginatorUserID, task.AgentID, true
	}
	a, ok := h.loadAgentForUser(w, r, agentID)
	if !ok {
		return empty, empty, empty, false
	}
	u, ok := parseUUIDOrBadRequest(w, user, "user id")
	return ws, u, a.ID, ok
}

func (h *Handler) CaptureNotificationEvent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
		service.NotificationEventInput
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, 400, "invalid notification event")
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "expected one notification object")
		return
	}
	ws, u, a, ok := h.notificationEventScope(w, r, req.AgentID)
	if !ok {
		return
	}
	if err := req.Facts.Validate(time.Now().UTC()); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	e, err := h.NotificationEvents.CaptureShadow(r.Context(), ws, u, a, req.NotificationEventInput)
	if errors.Is(err, service.ErrNotificationConflict) {
		writeError(w, 409, err.Error())
		return
	}
	if errors.Is(err, service.ErrNotificationInvalid) {
		writeError(w, 400, err.Error())
		return
	}
	if err != nil {
		writeError(w, 503, "notification capture unavailable; retry with the same event key")
		return
	}
	writeJSON(w, 200, map[string]any{"event": e, "mode": e.RouteOwner, "delivered": e.State == "sent"})
}

func (h *Handler) ownNotificationEvent(w http.ResponseWriter, r *http.Request) (db.NotificationEvent, bool) {
	ws, u, a, ok := h.notificationEventScope(w, r, r.URL.Query().Get("agent_id"))
	if !ok {
		return db.NotificationEvent{}, false
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "event id")
	if !ok {
		return db.NotificationEvent{}, false
	}
	e, err := h.Queries.GetNotificationEvent(r.Context(), db.GetNotificationEventParams{WorkspaceID: ws, ID: id})
	if err != nil || e.UserID != u || e.AgentID != a {
		writeError(w, 404, "notification event not found")
		return e, false
	}
	return e, true
}

func (h *Handler) GetNotificationEvent(w http.ResponseWriter, r *http.Request) {
	e, ok := h.ownNotificationEvent(w, r)
	if !ok {
		return
	}
	audit, err := h.Queries.ListNotificationDecisionAudit(r.Context(), db.ListNotificationDecisionAuditParams{WorkspaceID: e.WorkspaceID, EventID: e.ID})
	if err != nil {
		writeError(w, 503, "audit unavailable")
		return
	}
	var outbox any
	out, err := h.Queries.GetNotificationOutbox(r.Context(), db.GetNotificationOutboxParams{WorkspaceID: e.WorkspaceID, EventID: e.ID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 503, "outbox unavailable")
		return
	}
	if err == nil {
		outbox = out
	}
	writeJSON(w, 200, map[string]any{"event": e, "audit": audit, "outbox": outbox})
}

func (h *Handler) DecideNotificationEvent(w http.ResponseWriter, r *http.Request) {
	e, ok := h.ownNotificationEvent(w, r)
	if !ok {
		return
	}
	d, err := h.NotificationEvents.AuthorizeShadow(r.Context(), e.WorkspaceID, e.UserID, e.AgentID, e.ID)
	if err != nil {
		writeError(w, 503, "decision unavailable; event retained")
		return
	}
	writeJSON(w, 200, map[string]any{"decision": d, "mode": "shadow", "delivered": false})
}

func (h *Handler) AcknowledgeNotificationEvent(w http.ResponseWriter, r *http.Request) {
	e, ok := h.ownNotificationEvent(w, r)
	if !ok {
		return
	}
	// No caller-supplied session or author: bind to the currently authenticated
	// chat task, exactly as chat history does. Issue runs cannot import chat text.
	scope, ok := h.chatHistorySession(w, r)
	if !ok {
		return
	}
	var req struct {
		MessageID string `json:"message_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, 400, "invalid acknowledgement")
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "expected one acknowledgement object")
		return
	}
	message, ok := parseUUIDOrBadRequest(w, req.MessageID, "message id")
	if !ok {
		return
	}
	err := h.NotificationEvents.Acknowledge(r.Context(), e.WorkspaceID, e.UserID, e.AgentID, e.ID, scope.sessionID, message)
	if errors.Is(err, service.ErrNotificationAckSource) {
		writeError(w, 403, err.Error())
		return
	}
	if err != nil {
		writeError(w, 503, "acknowledgement not recorded")
		return
	}
	writeJSON(w, 200, map[string]any{"acknowledged": true, "event_id": uuidToString(e.ID)})
}
