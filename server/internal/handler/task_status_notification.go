package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Task-status notification subscriptions (MUL-11). A member opts in, for
// their own account only, to hearing from one agent in their private Telegram
// chat with it when an issue reaches blocked / in_review / done. Off by
// default; an agent can neither read nor change a subscription.

type TaskStatusNotificationSubscriptionResponse struct {
	AgentID        string   `json:"agent_id"`
	InstallationID string   `json:"installation_id"`
	Enabled        bool     `json:"enabled"`
	Statuses       []string `json:"statuses"`
	ProjectIDs     []string `json:"project_ids"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

func taskStatusNotificationSubscriptionToResponse(s db.TaskStatusNotificationSubscription) TaskStatusNotificationSubscriptionResponse {
	projects := make([]string, 0, len(s.ProjectIds))
	for _, p := range s.ProjectIds {
		projects = append(projects, uuidToString(p))
	}
	statuses := s.Statuses
	if statuses == nil {
		statuses = []string{}
	}
	return TaskStatusNotificationSubscriptionResponse{
		AgentID:        uuidToString(s.AgentID),
		InstallationID: uuidToString(s.InstallationID),
		Enabled:        s.Enabled,
		Statuses:       statuses,
		ProjectIDs:     projects,
		CreatedAt:      timestampToString(s.CreatedAt),
		UpdatedAt:      timestampToString(s.UpdatedAt),
	}
}

// requireMemberActor refuses agent-attributed requests: who is notified, and
// where, is the member's decision alone.
func (h *Handler) requireMemberActor(w http.ResponseWriter, r *http.Request, userID, workspaceID string) bool {
	if actorType, _ := h.resolveActor(r, userID, workspaceID); actorType != "member" {
		writeError(w, http.StatusForbidden, "only a member can manage their own notification subscriptions")
		return false
	}
	return true
}

func (h *Handler) ListTaskStatusNotificationSubscriptions(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	if !h.requireMemberActor(w, r, userID, workspaceID) {
		return
	}
	rows, err := h.Queries.ListTaskStatusNotificationSubscriptionsForUser(r.Context(), db.ListTaskStatusNotificationSubscriptionsForUserParams{
		WorkspaceID: parseUUID(workspaceID), UserID: parseUUID(userID),
	})
	if err != nil {
		slog.Warn("list task status notification subscriptions failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list notification subscriptions")
		return
	}
	out := make([]TaskStatusNotificationSubscriptionResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskStatusNotificationSubscriptionToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

type updateTaskStatusNotificationSubscriptionRequest struct {
	AgentID    string   `json:"agent_id"`
	Enabled    *bool    `json:"enabled"`
	Statuses   []string `json:"statuses"`
	ProjectIDs []string `json:"project_ids"`
}

// UpdateTaskStatusNotificationSubscription enables or disables the caller's
// own subscription to one agent. Enabling verifies the whole delivery route
// first — the agent's active Telegram bot, the caller's linked Telegram
// account, and a private chat the caller already opened with the bot — so a
// subscription that is on can actually be delivered.
func (h *Handler) UpdateTaskStatusNotificationSubscription(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	if !h.requireMemberActor(w, r, userID, workspaceID) {
		return
	}
	var req updateTaskStatusNotificationSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	agent, ok := h.loadAgentForUser(w, r, req.AgentID)
	if !ok {
		return
	}
	wsUUID, userUUID := parseUUID(workspaceID), parseUUID(userID)

	if !*req.Enabled {
		row, err := h.Queries.DisableTaskStatusNotificationSubscription(r.Context(), db.DisableTaskStatusNotificationSubscriptionParams{
			WorkspaceID: wsUUID, UserID: userUUID, AgentID: agent.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]any{"subscription": nil})
			return
		}
		if err != nil {
			slog.Warn("disable task status notification subscription failed", append(logger.RequestAttrs(r), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to update notification subscription")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"subscription": taskStatusNotificationSubscriptionToResponse(row)})
		return
	}

	statuses := req.Statuses
	if len(statuses) == 0 {
		statuses = service.TaskStatusNotificationStatuses
	}
	for _, s := range statuses {
		if !slices.Contains(service.TaskStatusNotificationStatuses, s) {
			writeError(w, http.StatusBadRequest, "statuses may only contain blocked, in_review and done")
			return
		}
	}
	projects := make([]pgtype.UUID, 0, len(req.ProjectIDs))
	for _, raw := range req.ProjectIDs {
		id, ok := parseUUIDOrBadRequest(w, raw, "project id")
		if !ok {
			return
		}
		if _, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{ID: id, WorkspaceID: wsUUID}); err != nil {
			writeError(w, http.StatusBadRequest, "project not found in this workspace")
			return
		}
		projects = append(projects, id)
	}

	binding, err := h.Queries.FindChannelBindingForMemberOnAgentInstallation(r.Context(), db.FindChannelBindingForMemberOnAgentInstallationParams{
		WorkspaceID: wsUUID, MulticaUserID: userUUID,
		ChannelType: service.TaskStatusNotificationChannelType, AgentID: agent.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "link your Telegram account with this agent's bot first",
			"reason": "telegram_account_not_linked",
		})
		return
	}
	if err != nil {
		slog.Warn("find telegram binding failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update notification subscription")
		return
	}
	if _, err := service.ResolveTaskStatusNotificationRoute(r.Context(), h.Queries, wsUUID, userUUID, agent.ID, binding.InstallationID, true); err != nil {
		var refused *service.TaskStatusNotificationRouteError
		if errors.As(err, &refused) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "this agent cannot reach you in a private Telegram chat yet; write to its bot directly first",
				"reason": refused.Reason,
			})
			return
		}
		slog.Warn("resolve notification route failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update notification subscription")
		return
	}
	row, err := h.Queries.UpsertTaskStatusNotificationSubscription(r.Context(), db.UpsertTaskStatusNotificationSubscriptionParams{
		WorkspaceID:    wsUUID,
		UserID:         userUUID,
		AgentID:        agent.ID,
		InstallationID: binding.InstallationID,
		Enabled:        true,
		Statuses:       statuses,
		ProjectIds:     projects,
	})
	if err != nil {
		slog.Warn("upsert task status notification subscription failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update notification subscription")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription": taskStatusNotificationSubscriptionToResponse(row)})
}
