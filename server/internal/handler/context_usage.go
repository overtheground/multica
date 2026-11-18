package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TaskContextPayload is the daemon's per-run context summary (MUL-197):
// the size of the context the model received on each call.
type TaskContextPayload struct {
	PeakTokens          int64 `json:"peak_tokens"`
	SumTokens           int64 `json:"sum_tokens"`
	Calls               int32 `json:"calls"`
	LastTokens          int64 `json:"last_tokens"`
	LastCacheReadTokens int64 `json:"last_cache_read_tokens"`
}

// ReportTaskContext stores the run's context summary. Idempotent: a repeat
// report overwrites the row.
func (h *Handler) ReportTaskContext(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	if _, ok := h.requireDaemonTaskAccess(w, r, taskID); !ok {
		return
	}
	var req TaskContextPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Calls <= 0 || req.PeakTokens <= 0 || req.SumTokens < 0 || req.LastTokens < 0 || req.LastCacheReadTokens < 0 {
		writeError(w, http.StatusBadRequest, "invalid context payload")
		return
	}
	if err := h.Queries.UpsertTaskContextUsage(r.Context(), db.UpsertTaskContextUsageParams{
		TaskID:              parseUUID(taskID),
		PeakContextTokens:   req.PeakTokens,
		SumContextTokens:    req.SumTokens,
		CallCount:           req.Calls,
		LastContextTokens:   req.LastTokens,
		LastCacheReadTokens: req.LastCacheReadTokens,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store task context")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ChatSessionContextResponse is the context the bot received on its latest
// answer in a chat. All zeros until the first answer reported one.
type ChatSessionContextResponse struct {
	ContextTokens   int64   `json:"context_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	PeakTokens      int64   `json:"peak_tokens"`
	UpdatedAt       *string `json:"updated_at"`
}

// GetChatSessionContext returns the latest reported context size of a chat.
func (h *Handler) GetChatSessionContext(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	row, err := h.Queries.GetChatSessionContext(r.Context(), session.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, ChatSessionContextResponse{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load chat context")
		return
	}
	updated := timestampToString(row.UpdatedAt)
	writeJSON(w, http.StatusOK, ChatSessionContextResponse{
		ContextTokens:   row.LastContextTokens,
		CacheReadTokens: row.LastCacheReadTokens,
		PeakTokens:      row.PeakContextTokens,
		UpdatedAt:       &updated,
	})
}

// DashboardContextRow is one agent's (or one day's) context aggregate. The
// average is derived client-side as SumTokens / Calls.
type DashboardContextRow struct {
	AgentID    string `json:"agent_id,omitempty"`
	Date       string `json:"date,omitempty"`
	Hour       *int32 `json:"hour,omitempty"`
	PeakTokens int64  `json:"peak_tokens"`
	SumTokens  int64  `json:"sum_tokens"`
	Calls      int64  `json:"calls"`
	TaskCount  int32  `json:"task_count"`
}

// GetDashboardContextByAgent returns per-agent peak and average context.
func (h *Handler) GetDashboardContextByAgent(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	projectID, ok := parseProjectIDParam(w, r)
	if !ok {
		return
	}
	restricted, ok := h.dashboardRestrictedAgents(w, r, workspaceID, member.Role)
	if !ok {
		return
	}
	tz := h.resolveViewingTZ(r)
	since := dashboardSince(r, 30, tz, true)
	rows, err := h.Queries.ListDashboardContextByAgent(r.Context(), db.ListDashboardContextByAgentParams{
		WorkspaceID: parseUUID(workspaceID),
		Since:       since,
		ProjectID:   projectID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list context by agent")
		return
	}
	resp := make([]DashboardContextRow, len(rows))
	for i, row := range rows {
		resp[i] = DashboardContextRow{
			AgentID:    uuidToString(row.AgentID),
			PeakTokens: row.PeakContextTokens,
			SumTokens:  row.SumContextTokens,
			Calls:      row.CallCount,
			TaskCount:  row.TaskCount,
		}
	}
	writeJSON(w, http.StatusOK, foldRestrictedAgents(
		resp,
		restricted,
		func(row DashboardContextRow) string { return row.AgentID },
		func(row DashboardContextRow) (DashboardContextRow, struct{}) {
			row.AgentID = restrictedAgentsRowID
			return row, struct{}{}
		},
		func(dst, src DashboardContextRow) DashboardContextRow {
			if src.PeakTokens > dst.PeakTokens {
				dst.PeakTokens = src.PeakTokens
			}
			dst.SumTokens += src.SumTokens
			dst.Calls += src.Calls
			dst.TaskCount += src.TaskCount
			return dst
		},
	))
}

// GetDashboardContextDaily returns the daily peak and average context.
func (h *Handler) GetDashboardContextDaily(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	projectID, ok := parseProjectIDParam(w, r)
	if !ok {
		return
	}
	tz := h.resolveViewingTZ(r)
	since := dashboardSince(r, 30, tz, false)
	rows, err := h.Queries.ListDashboardContextDaily(r.Context(), db.ListDashboardContextDailyParams{
		WorkspaceID: parseUUID(workspaceID),
		Tz:          tz,
		Since:       since,
		ProjectID:   projectID,
		Hourly:      hourlyRequested(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list daily context")
		return
	}
	resp := make([]DashboardContextRow, len(rows))
	for i, row := range rows {
		resp[i] = DashboardContextRow{
			Date:       row.Date.Time.Format("2006-01-02"),
			Hour:       bucketHour(row.Hour),
			PeakTokens: row.PeakContextTokens,
			SumTokens:  row.SumContextTokens,
			Calls:      row.CallCount,
			TaskCount:  row.TaskCount,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
