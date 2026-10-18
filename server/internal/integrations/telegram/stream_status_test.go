package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// statusCall is one Bot API request as the chat would see it.
type statusCall struct {
	method    string
	messageID int64
	text      string
}

// statusBot keeps every call in order, so a case can check each text the
// member saw rather than only the last one.
type statusBot struct {
	mu       sync.Mutex
	calls    []statusCall
	messages map[int64]string
	nextID   int64
}

func (b *statusBot) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	b.mu.Lock()
	defer b.mu.Unlock()
	text, _ := body["text"].(string)
	w.Header().Set("Content-Type", "application/json")
	switch method {
	case "sendMessage":
		b.nextID++
		b.messages[b.nextID] = text
		b.calls = append(b.calls, statusCall{method: method, messageID: b.nextID, text: text})
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, b.nextID)
	case "editMessageText":
		id := int64(body["message_id"].(float64))
		b.messages[id] = text
		b.calls = append(b.calls, statusCall{method: method, messageID: id, text: text})
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	default:
		b.calls = append(b.calls, statusCall{method: method})
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}
}

func (b *statusBot) snapshot() ([]statusCall, map[int64]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	messages := make(map[int64]string, len(b.messages))
	for id, text := range b.messages {
		messages[id] = text
	}
	return append([]statusCall(nil), b.calls...), messages
}

// statusQueries reports the task's queue status, which the real query would
// read from agent_task_queue.
type statusQueries struct {
	*fakeTelegramOutboundQueries
	mu         sync.Mutex
	taskStatus string
}

func (q *statusQueries) GetAgentTaskStatus(context.Context, pgtype.UUID) (db.GetAgentTaskStatusRow, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.taskStatus == "" {
		return db.GetAgentTaskStatusRow{}, pgx.ErrNoRows
	}
	return db.GetAgentTaskStatusRow{Status: q.taskStatus}, nil
}

func (q *statusQueries) setTaskStatus(status string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.taskStatus = status
}

func statusSetup(t *testing.T) (*Outbound, *statusQueries, *auditClock, *statusBot, events.Event) {
	t.Helper()
	bot := &statusBot{messages: make(map[int64]string)}
	srv := httptest.NewServer(http.HandlerFunc(bot.serve))
	t.Cleanup(srv.Close)
	base := newTelegramOutboundQueries(t)
	base.channelOrigin = true
	q := &statusQueries{fakeTelegramOutboundQueries: base, taskStatus: taskRunningStatus}
	o := NewOutbound(q, nil, srv.URL, srv.Client(), nil)
	c := &auditClock{}
	c.nanos.Store(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixNano())
	o.now = c.now
	o.wait = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
	o.spawn = func(f func()) { f() }
	return o, q, c, bot, telegramTestEvent()
}

func statusText(e events.Event, content string) events.Event {
	return telegramPartialEvent(e.TaskID, content)
}

// tick is one pass of the status ticker after the clock moved.
func tick(o *Outbound, c *auditClock, d time.Duration) {
	c.advance(d)
	o.refreshDueStatuses(context.Background())
}

// The reported symptom, frame for frame as the daemon log shows it for a Codex
// run: the first visible frame is the first token alone, the rest of the
// sentence arrives inside the chat's edit cooldown, and the run then goes
// quiet in a tool call. No call may ever show part of the sentence.
func TestStatusNeverShowsAPartialAgentMessage(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	sentence := "Покажу обе расшифровки и сравню их."

	o.handleTaskMessage(statusText(e, "П"))
	c.advance(time.Second)
	o.handleTaskMessage(statusText(e, strings.TrimPrefix(sentence, "П")))
	c.advance(100 * time.Millisecond)
	o.handleTaskMessage(telegramToolEvent(e.TaskID))
	for range 5 {
		tick(o, c, time.Second)
	}

	calls, messages := bot.snapshot()
	allowed := map[string]bool{
		statusPrefix + statusWorkingText: true,
		statusPrefix + sentence:          true,
	}
	for _, call := range calls {
		if !allowed[call.text] {
			t.Fatalf("Telegram was shown %q; calls=%+v", call.text, calls)
		}
	}
	if len(messages) != 1 || messages[1] != statusPrefix+sentence {
		t.Fatalf("status after the stage settled = %v, want one message with the whole sentence", messages)
	}
}

// An update that meets the cooldown is not dropped: the ticker sends it once
// the chat can take an edit, and not before.
func TestStatusDeferredUpdateLandsAfterCooldown(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Скачиваю файлы."))
	o.handleTaskMessage(telegramToolEvent(e.TaskID))
	if calls, _ := bot.snapshot(); len(calls) != 1 {
		t.Fatalf("the stage change inside the cooldown went out: %+v", calls)
	}
	tick(o, c, time.Second)
	if calls, _ := bot.snapshot(); len(calls) != 1 {
		t.Fatalf("the ticker edited inside the cooldown: %+v", calls)
	}
	tick(o, c, editInterval)
	calls, _ := bot.snapshot()
	if len(calls) != 2 || calls[1].method != "editMessageText" || calls[1].text != statusPrefix+"Скачиваю файлы." {
		t.Fatalf("deferred stage change = %+v", calls)
	}
	tick(o, c, editInterval)
	if calls, _ := bot.snapshot(); len(calls) != 2 {
		t.Fatalf("an unchanged status was edited again: %+v", calls)
	}
}

// Several stages inside one cooldown cost one edit, showing the latest.
func TestStatusCoalescesStagesWithinCooldown(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Первый этап."))
	o.handleTaskMessage(telegramToolEvent(e.TaskID))
	for _, stage := range []string{"Второй этап.", "Третий этап."} {
		c.advance(500 * time.Millisecond)
		o.handleTaskMessage(statusText(e, stage))
		o.handleTaskMessage(telegramToolEvent(e.TaskID))
	}
	tick(o, c, editInterval)
	calls, _ := bot.snapshot()
	if len(calls) != 2 || calls[1].text != statusPrefix+"Третий этап." {
		t.Fatalf("calls = %+v, want the first status and one edit to the latest stage", calls)
	}
}

// A long stage shows its age, once a minute, and only while the queue says
// the task is running: a clock is not evidence that the agent is alive.
func TestStatusShowsElapsedOnlyWhileTaskRuns(t *testing.T) {
	o, q, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Запускаю тесты."))
	o.handleTaskMessage(telegramToolEvent(e.TaskID))
	tick(o, c, editInterval)

	tick(o, c, time.Minute)
	_, messages := bot.snapshot()
	if want := statusPrefix + "Запускаю тесты.\n\nВсё ещё выполняется · 1 мин"; messages[1] != want {
		t.Fatalf("status after a minute = %q, want %q", messages[1], want)
	}
	before, _ := bot.snapshot()
	for range 20 {
		tick(o, c, time.Second)
	}
	if after, _ := bot.snapshot(); len(after) != len(before) {
		t.Fatalf("status re-edited within the same minute: %+v", after[len(before):])
	}
	tick(o, c, 40*time.Second)
	if _, messages := bot.snapshot(); !strings.HasSuffix(messages[1], "· 2 мин") {
		t.Fatalf("status after two minutes = %q", messages[1])
	}

	// The queue no longer says running — stalled in dispatch, or ended with
	// the terminal event still on its way. The status stops claiming progress.
	q.setTaskStatus("queued")
	tick(o, c, time.Minute)
	if _, messages := bot.snapshot(); strings.Contains(messages[1], "выполняется") {
		t.Fatalf("status claims a run the queue does not show running: %q", messages[1])
	}
}

func TestStatusElapsedFormatting(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Minute:                  "1 мин",
		59*time.Minute + time.Second: "59 мин",
		65 * time.Minute:             "1 ч 5 мин",
	} {
		if got := formatStatusElapsed(d); got != want {
			t.Errorf("formatStatusElapsed(%v) = %q, want %q", d, got, want)
		}
	}
}

// The final answer replaces the status in the same message, and nothing the
// status machinery does afterwards — the ticker, a late frame — touches it.
func TestStatusFinalAnswerReplacesStatusAndIsNeverOverwritten(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Сравниваю результаты."))
	o.handleTaskMessage(telegramToolEvent(e.TaskID))
	tick(o, c, editInterval)

	c.advance(editInterval)
	o.enqueueTerminalReply(e)
	auditDrain(t, o, c, e.ChatSessionID)

	o.handleTaskMessage(statusText(e, "Поздний кадр."))
	o.handleTaskMessage(telegramToolEvent(e.TaskID))
	for range 3 {
		tick(o, c, time.Minute)
	}
	calls, messages := bot.snapshot()
	if len(messages) != 1 || messages[1] != chatDoneContent(e.Payload) {
		t.Fatalf("messages = %v, want the status message carrying the final answer; calls=%+v", messages, calls)
	}
	if last := calls[len(calls)-1]; last.text != chatDoneContent(e.Payload) {
		t.Fatalf("a call after the final answer: %+v", calls)
	}
}

// A status edit already holding the turn when the answer arrives finishes
// first; the answer waits for the lease and lands after it.
func TestStatusEditInFlightCannotOutliveFinalAnswer(t *testing.T) {
	api, calls, sendStarted, release := gatedTelegramAPI(t)
	defer release()
	q := newTelegramOutboundQueries(t)
	q.channelOrigin = true
	o := NewOutbound(q, nil, api.URL, api.Client(), nil)
	e := telegramTestEvent()

	frameDone := make(chan struct{})
	go func() {
		defer close(frameDone)
		o.handleTaskMessage(statusText(e, "first"))
	}()
	<-sendStarted
	reply := &terminalReply{event: e, byteSize: len(chatDoneContent(e.Payload))}
	if result := o.sendNextTerminalRequest(context.Background(), reply); result.done || reply.initialized {
		t.Fatalf("final answer took a turn whose status send is in flight: %+v", result)
	}
	release()
	<-frameDone
	o.mu.Lock()
	schedule := o.streams[e.TaskID].schedule
	o.mu.Unlock()
	schedule.mu.Lock()
	schedule.lastEdit = time.Time{}
	schedule.mu.Unlock()
	if err := sendTerminalReplySynchronouslyForTest(context.Background(), o, e); err != nil {
		t.Fatalf("final answer: %v", err)
	}
	methods, bodies := calls()
	if strings.Join(methods, ",") != "sendMessage,editMessageText" || bodies[1]["text"] != chatDoneContent(e.Payload) {
		t.Fatalf("calls = %v %v, want the status and then the answer over it", methods, bodies)
	}
}

// A final answer over the message limit edits its first part into the status
// message and posts the rest after it.
func TestStatusLongFinalAnswerContinuesAfterStatusMessage(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Пишу отчёт."))
	content := strings.Repeat("слово ", maxMessageUnits/3)
	e.Payload = protocol.ChatDonePayload{TaskID: e.TaskID, ChatSessionID: e.ChatSessionID, Content: content}
	c.advance(editInterval)
	o.enqueueTerminalReply(e)
	auditDrain(t, o, c, e.ChatSessionID)

	calls, messages := bot.snapshot()
	chunks := chunkMessage(content, maxMessageUnits)
	if len(chunks) < 2 {
		t.Fatalf("setup: %d chunks", len(chunks))
	}
	if len(messages) != len(chunks) || messages[1] != formatHTML(chunks[0]) {
		t.Fatalf("messages = %d, want %d with the first part in the status message; calls=%d", len(messages), len(chunks), len(calls))
	}
	if calls[1].method != "editMessageText" || calls[1].messageID != 1 {
		t.Fatalf("second call = %+v, want the status edited into the first part", calls[1])
	}
}

// A failed run replaces the status with the notice, and the status stops.
func TestStatusFailureNoticeReplacesStatus(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Проверяю."))
	o.handleTaskMessage(telegramToolEvent(e.TaskID))
	c.advance(editInterval)
	failed := e
	failed.Type = protocol.EventTaskFailed
	failed.Payload = map[string]any{"retry_pending": false}
	o.handleTaskFailed(failed)
	auditDrain(t, o, c, e.ChatSessionID)
	for range 3 {
		tick(o, c, time.Minute)
	}
	_, messages := bot.snapshot()
	if len(messages) != 1 || messages[1] != taskFailedText {
		t.Fatalf("messages = %v, want the status replaced by the failure notice", messages)
	}
}

// A cancelled run says it stopped instead of leaving a status that claims it
// is still working; a late frame does not reopen the turn.
func TestStatusCancellationRewritesStatus(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Собираю данные."))
	o.handleTaskMessage(telegramToolEvent(e.TaskID))
	c.advance(editInterval)
	o.handleTaskCancelled(e)
	auditDrain(t, o, c, e.ChatSessionID)
	o.handleTaskMessage(statusText(e, "late"))
	o.handleTaskMessage(telegramToolEvent(e.TaskID))
	tick(o, c, time.Minute)

	_, messages := bot.snapshot()
	if len(messages) != 1 || messages[1] != cancelledText {
		t.Fatalf("messages = %v, want the status rewritten to %q", messages, cancelledText)
	}
}

// A run that ends with no answer marks the status done rather than leaving it
// "working" forever.
func TestStatusEmptyCompletionRewritesStatus(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Думаю."))
	c.advance(editInterval)
	empty := e
	empty.Payload = protocol.ChatDonePayload{TaskID: e.TaskID, ChatSessionID: e.ChatSessionID}
	o.enqueueTerminalReply(empty)
	auditDrain(t, o, c, e.ChatSessionID)
	_, messages := bot.snapshot()
	if len(messages) != 1 || messages[1] != emptyReplyText {
		t.Fatalf("messages = %v, want the status rewritten to %q", messages, emptyReplyText)
	}
}

// A cancellation before any status reached the chat posts nothing at all.
func TestStatusCancellationBeforeFirstStatusPostsNothing(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskCancelled(e)
	auditDrain(t, o, c, e.ChatSessionID)
	o.handleTaskMessage(statusText(e, "late"))
	tick(o, c, time.Minute)
	if calls, _ := bot.snapshot(); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
}

// After a restart the process has no local state, but the turn's message is
// recorded: the next status edits it instead of posting a second one, and the
// final answer still lands there.
func TestStatusAfterRestartEditsTheRecordedMessage(t *testing.T) {
	o, q, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "До рестарта."))
	o.handleTaskMessage(telegramToolEvent(e.TaskID))

	restarted := NewOutbound(q, nil, o.apiBase, o.client, nil)
	restarted.now, restarted.wait, restarted.spawn = o.now, o.wait, o.spawn
	c.advance(editInterval)
	restarted.handleTaskMessage(statusText(e, "После рестарта."))
	restarted.handleTaskMessage(telegramToolEvent(e.TaskID))
	tick(restarted, c, editInterval)
	c.advance(editInterval)
	restarted.enqueueTerminalReply(e)
	auditDrain(t, restarted, c, e.ChatSessionID)

	calls, messages := bot.snapshot()
	if len(messages) != 1 || messages[1] != chatDoneContent(e.Payload) {
		t.Fatalf("messages = %v, want one message ending with the answer; calls=%+v", messages, calls)
	}
	for _, call := range calls[1:] {
		if call.method != "editMessageText" {
			t.Fatalf("the restarted process posted a second message: %+v", calls)
		}
	}
}

// Only whole, user-facing text reaches the status: tool output never does.
func TestStatusIgnoresToolOutput(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Смотрю конфиг."))
	o.handleTaskMessage(events.Event{TaskID: e.TaskID, Type: protocol.EventTaskMessage,
		Payload: protocol.TaskMessagePayload{TaskID: e.TaskID, Type: "tool_result", Output: "TOKEN=secret-value"}})
	tick(o, c, editInterval)
	calls, _ := bot.snapshot()
	for _, call := range calls {
		if strings.Contains(call.text, "secret-value") {
			t.Fatalf("tool output reached Telegram: %+v", calls)
		}
	}
	if _, messages := bot.snapshot(); messages[1] != statusPrefix+"Смотрю конфиг." {
		t.Fatalf("status = %q", messages[1])
	}
}

func TestStatusCommentExcerpt(t *testing.T) {
	if got := statusComment("  Короткий статус.  "); got != "Короткий статус." {
		t.Fatalf("short comment = %q", got)
	}
	sentence := strings.Repeat("а", 250) + ". "
	long := sentence + strings.Repeat("слово ", 100)
	got := statusComment(long)
	if got != strings.TrimSpace(sentence)+" …" {
		t.Fatalf("long comment cut = %q, want the first sentence", got)
	}
	words := strings.Repeat("слово ", 100)
	got = statusComment(words)
	if !strings.HasSuffix(got, "слово …") || strings.Count(got, "слово") > statusCommentMaxRunes/6+1 {
		t.Fatalf("word cut = %q", got)
	}
	fenced := "Запускаю:\n```\n" + strings.Repeat("x ", 300) + "\n```"
	if got := statusComment(fenced); strings.Contains(got, "```") {
		t.Fatalf("cut left a code fence open: %q", got)
	}
}

// Once the final answer has started, the status never comes back — not even
// when the answer's lease lapses between its parts, as it does when the
// process delivering it dies. A status taking that turn would overwrite the
// part of the answer already in the chat.
func TestStatusCannotReclaimAPartlyDeliveredAnswer(t *testing.T) {
	o, _, c, bot, e := statusSetup(t)
	o.handleTaskMessage(statusText(e, "Пишу отчёт."))
	content := strings.Repeat("слово ", maxMessageUnits/3)
	e.Payload = protocol.ChatDonePayload{TaskID: e.TaskID, ChatSessionID: e.ChatSessionID, Content: content}
	c.advance(editInterval)

	ctx := context.Background()
	reply := &terminalReply{event: e, byteSize: len(content)}
	for reply.chunkIndex == 0 {
		result := o.sendNextTerminalRequest(ctx, reply)
		if result.done {
			t.Fatalf("answer finished in one part: %+v", result)
		}
		c.advance(result.retryAt.Sub(c.now()))
	}
	o.releaseDelivery(ctx, reply.lease)
	reply.lease = nil

	// Local state as the answer left it, or as a new process would find it.
	other := NewOutbound(o.q, nil, o.apiBase, o.client, nil)
	other.now, other.wait, other.spawn = o.now, o.wait, o.spawn
	other.handleTaskMessage(statusText(e, "Поздний этап."))
	other.handleTaskMessage(telegramToolEvent(e.TaskID))
	tick(other, c, time.Minute)

	_, messages := bot.snapshot()
	if want := formatHTML(chunkMessage(content, maxMessageUnits)[0]); messages[1] != want {
		t.Fatalf("the first part of the answer was overwritten by a status: %q", messages[1])
	}
}
