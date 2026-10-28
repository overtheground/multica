package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (h *Handler) GetNotificationTopic(w http.ResponseWriter, r *http.Request) {
	ws, u, a, ok := h.notificationEventScope(w, r, r.URL.Query().Get("agent_id"))
	if !ok {
		return
	}
	producer := r.URL.Query().Get("producer")
	if producer != "subscription-limits" {
		writeError(w, 400, "unsupported producer")
		return
	}
	route, err := h.Queries.GetNotificationTopicRoute(r.Context(), db.GetNotificationTopicRouteParams{WorkspaceID: ws, UserID: u, AgentID: a, Producer: producer})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, 200, map[string]any{"route": nil, "revision": 0, "audit": []any{}})
		return
	}
	if err != nil {
		writeError(w, 503, "topic store unavailable")
		return
	}
	audit, err := h.Queries.ListNotificationTopicRouteAudit(r.Context(), db.ListNotificationTopicRouteAuditParams{WorkspaceID: ws, RouteID: route.ID})
	if err != nil {
		writeError(w, 503, "topic audit unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"route": route, "revision": route.Revision, "audit": audit})
}
func (h *Handler) ConfigureNotificationTopic(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, 403, "only a human may configure notification consent")
		return
	}
	var req struct {
		AgentID          string `json:"agent_id"`
		Producer         string `json:"producer"`
		InstallationID   string `json:"installation_id"`
		ExpectedRevision int64  `json:"expected_revision"`
		Enabled          bool   `json:"enabled"`
		Owner            string `json:"route_owner"`
		LegacyStopped    bool   `json:"legacy_stopped"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, 400, "invalid topic configuration")
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "expected one configuration object")
		return
	}
	ws, u, a, ok := h.notificationEventScope(w, r, req.AgentID)
	if !ok {
		return
	}
	installation, ok := parseUUIDOrBadRequest(w, req.InstallationID, "installation id")
	if !ok {
		return
	}
	route, err := h.NotificationEvents.ConfigureTopic(r.Context(), ws, u, a, req.Producer, service.NotificationTopicInput{InstallationID: installation, ExpectedRevision: req.ExpectedRevision, Enabled: req.Enabled, Owner: req.Owner, LegacyStopped: req.LegacyStopped})
	var refused *service.TaskStatusNotificationRouteError
	switch {
	case errors.Is(err, service.ErrNotificationInvalid):
		writeError(w, 400, "invalid topic configuration")
	case errors.Is(err, service.ErrNotificationConflict):
		writeError(w, 409, "topic revision changed; read current route before retry")
	case errors.As(err, &refused):
		writeError(w, 409, refused.Reason)
	case err != nil:
		writeError(w, 503, "topic configuration unavailable")
	default:
		writeJSON(w, 200, map[string]any{"route": route, "revision": route.Revision})
	}
}
func (h *Handler) RollbackNotificationEvent(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, 403, "only a human may transfer notification ownership")
		return
	}
	e, ok := h.ownNotificationEvent(w, r)
	if !ok {
		return
	}
	err := h.NotificationEvents.RollbackDirect(r.Context(), e.WorkspaceID, e.ID)
	if errors.Is(err, service.ErrNotificationConflict) {
		writeError(w, 409, "only a pending event with zero delivered chunks may be returned to shadow")
		return
	}
	if err != nil {
		writeError(w, 503, "rollback unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"route_owner": "shadow"})
}
