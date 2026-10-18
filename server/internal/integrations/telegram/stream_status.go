package telegram

// stream_status.go — the status message a Telegram turn shows while it runs.
//
// The reply used to stream the agent's raw transcript into its placeholder.
// The daemon flushes the first visible frame immediately, and a backend that
// streams token deltas (Codex) makes that frame the first token alone — one
// letter. The next frame then landed inside the chat's edit cooldown and was
// dropped with nothing scheduled to send it later, so a run that went on into
// tool calls showed that letter until the final answer replaced it.
//
// The message is now a status, rebuilt from whole pieces only:
//
//   - a neutral line until the agent has finished saying something;
//   - the agent's last complete message, once a non-text frame (a tool call, a
//     tool result, thinking) proves the message ended — text still arriving is
//     never shown, since it may stop mid-word;
//   - how long the current stage has run, once it passes a minute, and only
//     while the task is really running according to the database.
//
// Every edit is made under the turn's streaming lease, so the final answer,
// the failure notice and the cancellation close — which take the turn in the
// terminal phase, which a streaming acquire can never take back — always win.
// A ticker sends what the bus path could not: an update that met the chat's
// edit cooldown, and the elapsed time of a stage with no frames at all.

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	// statusTickInterval paces the ticker. A tick that finds nothing due costs
	// one pass over the in-memory streams; it does not touch the database.
	statusTickInterval = time.Second
	// statusElapsedAfter is how long a stage runs before its age is shown.
	statusElapsedAfter = time.Minute
	// statusRetryDelay spaces attempts after an edit or send Telegram refused
	// for a reason other than a 429, which carries its own retry_after.
	statusRetryDelay = 10 * time.Second
	// statusCommentMaxRunes bounds the agent message shown as the stage. A
	// status is a line or two; a longer message is cut at a sentence or word
	// boundary, never mid-word.
	statusCommentMaxRunes = 400
	// statusRefreshTimeout bounds one refresh started by the ticker.
	statusRefreshTimeout = 10 * time.Second
)

const (
	statusPrefix      = "⏳ "
	statusWorkingText = "Работаю над ответом…"
	statusElapsedText = "Всё ещё выполняется · %s"
	// cancelledText replaces the status when the run is cancelled, and
	// emptyReplyText when it finishes without an answer to show. Left alone,
	// a status would claim forever that the run is still working.
	cancelledText  = "⏹ Остановлено."
	emptyReplyText = "✅ Готово."
)

// taskRunningStatus is the agent_task_queue status of a run in progress.
const taskRunningStatus = "running"

// statusFrame folds one transcript frame into the stream's status. It reports
// whether the status the user should see changed. Callers hold o.mu.
func statusFrame(st *streamState, payload protocol.TaskMessagePayload, now time.Time) bool {
	if payload.Type == "text" {
		st.segment += payload.Content
		return false
	}
	// Anything but text ends the agent's current message: the daemon only
	// starts another frame type once that message is over.
	comment := statusComment(st.segment)
	st.segment = ""
	if comment == "" || comment == st.comment {
		return false
	}
	st.comment = comment
	st.stageAt = now
	st.version++
	return true
}

// statusComment is the complete agent message shown as the current stage,
// trimmed to a status-sized excerpt.
func statusComment(segment string) string {
	text := strings.TrimSpace(segment)
	if text == "" {
		return ""
	}
	if utf8.RuneCountInString(text) <= statusCommentMaxRunes {
		return text
	}
	runes := []rune(text)[:statusCommentMaxRunes]
	cut := len(runes)
	// Prefer the end of a sentence in the second half; otherwise the last
	// word boundary. Either way the excerpt never ends inside a word.
	for i := len(runes) - 1; i >= statusCommentMaxRunes/2; i-- {
		if r := runes[i]; r == '.' || r == '!' || r == '?' || r == '…' || r == '\n' {
			cut = i + 1
			break
		}
	}
	if cut == len(runes) {
		for i := len(runes) - 1; i > 0; i-- {
			if unicode.IsSpace(runes[i]) {
				cut = i
				break
			}
		}
	}
	excerpt := strings.TrimSpace(string(runes[:cut]))
	if strings.Count(excerpt, "```")%2 == 1 {
		// Cutting inside a code fence would leave it open; drop the fence.
		excerpt = strings.TrimSpace(excerpt[:strings.LastIndex(excerpt, "```")])
	}
	return excerpt + " …"
}

// renderStatus is the markdown of the status for the stream's current state.
// running is the task's database status, not an inference from the clock: the
// elapsed line says the run is still going, and only the queue can say that.
func renderStatus(st *streamState, now time.Time, running bool) string {
	text := statusPrefix + statusWorkingText
	if st.comment != "" {
		text = statusPrefix + st.comment
	}
	if elapsed := now.Sub(st.stageAt); running && elapsed >= statusElapsedAfter {
		text += "\n\n" + fmt.Sprintf(statusElapsedText, formatStatusElapsed(elapsed))
	}
	return text
}

func formatStatusElapsed(d time.Duration) string {
	minutes := int(d / time.Minute)
	if minutes < 60 {
		return fmt.Sprintf("%d мин", minutes)
	}
	return fmt.Sprintf("%d ч %d мин", minutes/60, minutes%60)
}

// statusBucket is the elapsed-time granularity the status shows, so a stage
// is re-rendered once a minute rather than on every tick.
func statusBucket(st *streamState, now time.Time) int {
	return int(now.Sub(st.stageAt) / time.Minute)
}

// statusDueLocked reports whether the stream has something to publish.
// Callers hold o.mu.
func statusDueLocked(st *streamState, now time.Time) bool {
	if st.frozen || st.refreshing || st.retryAt.After(now) {
		return false
	}
	return st.version != st.shownVersion || statusBucket(st, now) != st.shownBucket
}

// refreshStatus publishes the stream's status if it changed: the first call
// sends the message, later ones edit it. It makes at most one Telegram call,
// under the turn's streaming lease.
func (o *Outbound) refreshStatus(ctx context.Context, key string, st *streamState) {
	o.mu.Lock()
	if o.streams[key] != st || st.refreshing || st.frozen {
		o.mu.Unlock()
		return
	}
	st.refreshing = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		st.refreshing = false
		o.mu.Unlock()
	}()

	// The cooldown is checked before any database work: a throttled update
	// is the common case while an agent is busy, and the ticker comes back.
	if !o.statusSlotOpen(st.schedule) {
		return
	}
	target, err := o.resolveTarget(ctx, events.Event{TaskID: key}, true)
	if err != nil {
		return
	}
	if target == nil {
		// The installation was revoked mid-run: nothing may be sent.
		o.clearStream(events.Event{TaskID: key})
		return
	}
	lease, status, err := o.acquireDelivery(ctx, target, st.turn, deliveryPhaseStreaming)
	if err != nil {
		o.logger.WarnContext(ctx, "telegram outbound: reply ownership unavailable; status update deferred", "error", err)
		return
	}
	if status == deliveryClosed {
		// Answered, failed, cancelled or superseded — possibly by another
		// replica. Nothing more is ever shown for this attempt.
		o.clearStream(events.Event{TaskID: key})
		return
	}
	if status != deliveryAcquired {
		return
	}
	defer o.releaseDelivery(ctx, lease)
	if o.inheritedSend(ctx, lease) {
		// A send nobody can account for may already be in the chat; the final
		// answer decides what to do about it, the status stops.
		o.freezeStatus(st)
		return
	}

	o.mu.Lock()
	needsAge := o.now().Sub(st.stageAt) >= statusElapsedAfter
	o.mu.Unlock()
	running := false
	if needsAge {
		if row, err := o.q.GetAgentTaskStatus(ctx, target.taskID); err == nil {
			running = row.Status == taskRunningStatus
		}
	}

	if !st.schedule.mu.TryLock() {
		return
	}
	defer st.schedule.mu.Unlock()
	now := o.now()
	if o.terminalAvailableAt(st.schedule, target.botKey, now).After(now) {
		return
	}

	o.mu.Lock()
	if o.streams[key] != st {
		o.mu.Unlock()
		return
	}
	if msgID := lease.messageID(); msgID != 0 {
		// A message this process did not send — another replica, or the
		// attempt an automatic retry inherited — is edited, never doubled.
		st.messageID = msgID
	}
	msgID := st.messageID
	version := st.version
	bucket := statusBucket(st, now)
	text := renderStatus(st, now, running)
	plain := st.plainText
	unchanged := msgID != 0 && text == st.shownText
	if unchanged {
		st.shownVersion, st.shownBucket = version, bucket
	}
	o.mu.Unlock()
	if unchanged {
		return
	}

	body, parseMode := formatHTML(text), "HTML"
	if plain {
		body, parseMode = text, ""
	}
	api := newBotAPI(o.apiBase, target.botToken, o.client)
	if msgID != 0 {
		// Re-prove the turn immediately before the edit: an edit made after
		// the lease lapsed could overwrite the final answer.
		if !o.renewDelivery(ctx, lease) {
			return
		}
		callCtx, cancel := o.callContext(ctx)
		defer cancel()
		err := api.EditMessageText(callCtx, editMessageTextParams{
			ChatID: target.chatID, MessageID: msgID, Text: body, ParseMode: parseMode,
		})
		if err != nil && !isNotModified(err) {
			o.noteStatusFailure(st, err, now)
			return
		}
		st.schedule.lastEdit = o.now()
		o.markStatusShown(st, text, version, bucket)
		return
	}

	// Exactly one message per turn, published before the call goes out so
	// that any other process reads "a send is outstanding".
	if !o.claimSend(ctx, lease) {
		return
	}
	callCtx, cancel := o.callContext(ctx)
	defer cancel()
	m, err := api.SendMessage(callCtx, sendMessageParams{
		ChatID: target.chatID, Text: body, ParseMode: parseMode,
		MessageThreadID: target.threadID,
		ReplyParameters: optionalReplyParameters(target.replyTo),
	})
	switch o.recordSend(ctx, lease, true, m.MessageID, 0, err) {
	case deliveryAccepted:
	case deliveryUnknown:
		o.freezeStatus(st)
		return
	default:
		o.noteStatusFailure(st, err, now)
		return
	}
	st.schedule.lastEdit = o.now()
	o.mu.Lock()
	st.messageID = m.MessageID
	o.mu.Unlock()
	o.markStatusShown(st, text, version, bucket)
}

// statusSlotOpen reports whether the chat could take a call right now.
func (o *Outbound) statusSlotOpen(schedule *chatSchedule) bool {
	if !schedule.mu.TryLock() {
		return false
	}
	defer schedule.mu.Unlock()
	now := o.now()
	return !o.terminalAvailableAt(schedule, schedule.key.botKey, now).After(now)
}

func (o *Outbound) markStatusShown(st *streamState, text string, version, bucket int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st.shownText, st.shownVersion, st.shownBucket = text, version, bucket
}

func (o *Outbound) freezeStatus(st *streamState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st.frozen = true
}

// noteStatusFailure decides when, if ever, the status is tried again. The
// final answer lands through its own path whatever happens here.
func (o *Outbound) noteStatusFailure(st *streamState, err error, now time.Time) {
	if wait, ok := retryAfter(err); ok {
		st.schedule.setBackoffTill(now.Add(wait))
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case isHTMLParseError(err) && !st.plainText:
		// Telegram refused the markup, not the message: same text, plain.
		st.plainText = true
	case isEditTargetMissing(err) || isPermanentEditRejection(err):
		// Deleted by the user, or no longer editable: retrying cannot help.
		st.frozen = true
		o.logger.Warn("telegram outbound: status message can no longer be updated", "error", err)
	default:
		st.retryAt = now.Add(statusRetryDelay)
		o.logger.Warn("telegram outbound: status update failed", "error", err)
	}
}

// runStatusTicker publishes status updates no frame is coming to trigger.
func (o *Outbound) runStatusTicker(ctx context.Context) {
	defer o.workerWG.Done()
	ticker := time.NewTicker(statusTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.refreshDueStatuses(ctx)
		}
	}
}

// refreshDueStatuses starts a refresh for every stream with something to
// show. Each runs on its own, so one slow Telegram call does not hold back
// the other chats.
func (o *Outbound) refreshDueStatuses(ctx context.Context) {
	type due struct {
		key    string
		stream *streamState
	}
	now := o.now()
	var pending []due
	o.mu.Lock()
	for key, st := range o.streams {
		if statusDueLocked(st, now) {
			pending = append(pending, due{key: key, stream: st})
		}
	}
	o.mu.Unlock()
	for _, d := range pending {
		o.workerWG.Add(1)
		o.spawn(func() {
			defer o.workerWG.Done()
			refreshCtx, cancel := context.WithTimeout(ctx, statusRefreshTimeout)
			defer cancel()
			o.refreshStatus(refreshCtx, d.key, d.stream)
		})
	}
}
