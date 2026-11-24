package handler

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgtype"
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

// hydrateTaskContextTokens fills ContextTokens on run listings (MUL-207). The
// size is optional metadata, so a failed read leaves the field unset.
func (h *Handler) hydrateTaskContextTokens(ctx context.Context, resp []AgentTaskResponse) {
	if len(resp) == 0 {
		return
	}
	ids := make([]pgtype.UUID, len(resp))
	for i := range resp {
		ids[i] = parseUUID(resp[i].ID)
	}
	rows, err := h.Queries.ListTaskContextTokens(ctx, ids)
	if err != nil {
		return
	}
	byTask := make(map[string]int64, len(rows))
	for _, row := range rows {
		byTask[uuidToString(row.TaskID)] = row.LastContextTokens
	}
	for i := range resp {
		if v, ok := byTask[resp[i].ID]; ok {
			resp[i].ContextTokens = &v
		}
	}
}

// issueSessionDropped reports whether this issue run must start a fresh
// session although a resumable one exists (MUL-207): the agent runs fresh on
// every run, or the newest reported context of (agent, issue) is above the
// agent's cap. No cap and no flag, the default, never drops anything.
func (h *Handler) issueSessionDropped(ctx context.Context, task *db.AgentTaskQueue) bool {
	agent, err := h.Queries.GetAgent(ctx, task.AgentID)
	if err != nil {
		return false
	}
	return sessionDropped(agent.FreshSessionPerRun, agent.ContextCapTokens, func() (int64, bool) {
		n, err := h.Queries.GetLastIssueContextTokens(ctx, db.GetLastIssueContextTokensParams{
			AgentID: task.AgentID,
			IssueID: task.IssueID,
		})
		return n, err == nil
	})
}

// sessionDropped is the pure decision behind issueSessionDropped.
func sessionDropped(fresh bool, capTokens pgtype.Int8, lastContext func() (int64, bool)) bool {
	if fresh {
		return true
	}
	if !capTokens.Valid {
		return false
	}
	n, ok := lastContext()
	return ok && n > capTokens.Int64
}
