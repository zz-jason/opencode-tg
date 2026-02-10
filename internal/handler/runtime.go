package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"tg-bot/internal/opencode"

	log "github.com/sirupsen/logrus"
	"gopkg.in/telebot.v4"
)

const (
	runtimeEventQueueSize    = 2048
	runtimeSubmitQueueSize   = 8
	runtimeFlushInterval     = 350 * time.Millisecond
	runtimeSettleGrace       = 1200 * time.Millisecond
	runtimeNoOutputGrace     = 15 * time.Second
	runtimeReconcileInterval = 1200 * time.Millisecond
	runtimeBootstrapTimeout  = 8 * time.Second
)

type openCodeRuntime struct {
	bot    *Bot
	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup

	actorsMu sync.RWMutex
	actors   map[string]*sessionActor

	statusMu      sync.RWMutex
	sessionStatus map[string]opencode.SessionStatusInfo
}

type runtimeTaskRequest struct {
	SessionID      string
	RequestTraceID string
	Text           string
	Model          *opencode.MessageModel
	TelegramCtx    telebot.Context
}

type sessionActor struct {
	runtime   *openCodeRuntime
	bot       *Bot
	sessionID string

	submitCh chan *actorSubmitRequest
	eventCh  chan opencode.SessionEvent
}

type actorSubmitRequest struct {
	task     runtimeTaskRequest
	resultCh chan error
}

type actorRunningTask struct {
	req  *actorSubmitRequest
	done bool

	startedAt time.Time
	deadline  time.Time

	idleReconciled bool
	state          *streamingState
}

func newOpenCodeRuntime(parent context.Context, bot *Bot) (*openCodeRuntime, error) {
	ctx, cancel := context.WithCancel(parent)
	runtime := &openCodeRuntime{
		bot:           bot,
		ctx:           ctx,
		cancel:        cancel,
		actors:        make(map[string]*sessionActor),
		sessionStatus: make(map[string]opencode.SessionStatusInfo),
	}

	runtime.wg.Add(1)
	go runtime.runEventPump()

	if err := runtime.bootstrapBlocking(); err != nil {
		runtime.cancel()
		runtime.wg.Wait()
		return nil, err
	}

	runtime.wg.Add(1)
	go runtime.bootstrapNonBlocking()
	return runtime, nil
}

func (r *openCodeRuntime) Close() {
	if r == nil {
		return
	}
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
}

func (r *openCodeRuntime) SubmitTextTask(req runtimeTaskRequest) error {
	if strings.TrimSpace(req.SessionID) == "" {
		return fmt.Errorf("empty session id")
	}
	if strings.TrimSpace(req.Text) == "" {
		return fmt.Errorf("empty task text")
	}
	if req.TelegramCtx == nil {
		return fmt.Errorf("nil telegram context")
	}

	actor := r.getOrCreateActor(req.SessionID)
	return actor.submit(req)
}

func (r *openCodeRuntime) bootstrapBlocking() error {
	start := time.Now()
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{
			name: "GET /session",
			run: func(ctx context.Context) error {
				_, err := r.bot.opencodeClient.ListSessions(ctx)
				return err
			},
		},
		{
			name: "GET /provider",
			run: func(ctx context.Context) error {
				_, err := r.bot.opencodeClient.GetProviders(ctx)
				return err
			},
		},
		{
			name: "GET /agent",
			run: func(ctx context.Context) error {
				_, err := r.bot.opencodeClient.GetAgents(ctx)
				return err
			},
		},
		{
			name: "GET /config",
			run: func(ctx context.Context) error {
				_, err := r.bot.opencodeClient.GetConfig(ctx)
				return err
			},
		},
	}

	for _, step := range steps {
		stepCtx, cancel := context.WithTimeout(r.ctx, runtimeBootstrapTimeout)
		err := step.run(stepCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("runtime bootstrap failed at %s: %w", step.name, err)
		}
	}

	log.Infof("OpenCode runtime bootstrap (blocking) completed in %v", time.Since(start))
	return nil
}

func (r *openCodeRuntime) bootstrapNonBlocking() {
	defer r.wg.Done()
	start := time.Now()

	statusCtx, cancelStatus := context.WithTimeout(r.ctx, runtimeBootstrapTimeout)
	statuses, statusErr := r.bot.opencodeClient.GetSessionStatus(statusCtx)
	cancelStatus()
	if statusErr != nil {
		log.Warnf("OpenCode runtime non-blocking bootstrap failed at GET /session/status: %v", statusErr)
	} else {
		r.statusMu.Lock()
		for sessionID, status := range statuses {
			r.sessionStatus[sessionID] = status
		}
		r.statusMu.Unlock()
	}

	commandCtx, cancelCommands := context.WithTimeout(r.ctx, runtimeBootstrapTimeout)
	if _, err := r.bot.opencodeClient.GetCommands(commandCtx); err != nil {
		log.Warnf("OpenCode runtime non-blocking bootstrap failed at GET /command: %v", err)
	}
	cancelCommands()

	log.Infof("OpenCode runtime bootstrap (non-blocking) completed in %v", time.Since(start))
}

func (r *openCodeRuntime) runEventPump() {
	defer r.wg.Done()

	backoff := 200 * time.Millisecond
	for {
		if r.ctx.Err() != nil {
			return
		}

		err := r.bot.opencodeClient.StreamSessionEvents(r.ctx, func(event opencode.SessionEvent) error {
			r.routeEvent(event)
			return nil
		})
		if r.ctx.Err() != nil {
			return
		}

		if err != nil {
			log.Warnf("OpenCode runtime event pump disconnected: %v", err)
		} else {
			log.Warn("OpenCode runtime event pump closed unexpectedly")
		}

		select {
		case <-r.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 3*time.Second {
			backoff *= 2
			if backoff > 3*time.Second {
				backoff = 3 * time.Second
			}
		}
	}
}

func (r *openCodeRuntime) routeEvent(event opencode.SessionEvent) {
	if event.Type == "server.connected" || event.Type == "server.heartbeat" {
		return
	}

	sessionID, status := sessionEventSessionIDAndStatus(event)
	if sessionID == "" {
		return
	}
	if status != nil {
		r.statusMu.Lock()
		r.sessionStatus[sessionID] = *status
		r.statusMu.Unlock()
	}

	actor := r.getActor(sessionID)
	if actor == nil {
		return
	}
	actor.publishEvent(event)
}

func (r *openCodeRuntime) getActor(sessionID string) *sessionActor {
	r.actorsMu.RLock()
	defer r.actorsMu.RUnlock()
	return r.actors[sessionID]
}

func (r *openCodeRuntime) getOrCreateActor(sessionID string) *sessionActor {
	r.actorsMu.Lock()
	defer r.actorsMu.Unlock()

	if actor, ok := r.actors[sessionID]; ok {
		return actor
	}

	actor := &sessionActor{
		runtime:   r,
		bot:       r.bot,
		sessionID: sessionID,
		submitCh:  make(chan *actorSubmitRequest, runtimeSubmitQueueSize),
		eventCh:   make(chan opencode.SessionEvent, runtimeEventQueueSize),
	}
	r.actors[sessionID] = actor

	r.wg.Add(1)
	go actor.run()
	return actor
}

func (a *sessionActor) publishEvent(event opencode.SessionEvent) {
	select {
	case a.eventCh <- event:
	case <-a.runtime.ctx.Done():
	case <-time.After(2 * time.Second):
		log.Warnf("Dropping OpenCode event due to actor backpressure: session=%s type=%s", a.sessionID, event.Type)
	}
}

func (a *sessionActor) submit(task runtimeTaskRequest) error {
	req := &actorSubmitRequest{
		task:     task,
		resultCh: make(chan error, 1),
	}

	select {
	case <-a.runtime.ctx.Done():
		return fmt.Errorf("runtime closed")
	case a.submitCh <- req:
	}

	select {
	case <-a.runtime.ctx.Done():
		return fmt.Errorf("runtime closed")
	case err := <-req.resultCh:
		return err
	}
}

func (a *sessionActor) run() {
	defer a.runtime.wg.Done()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var current *actorRunningTask
	for {
		select {
		case <-a.runtime.ctx.Done():
			if current != nil {
				a.finishTask(current, a.runtime.ctx.Err())
			}
			return

		case submitReq := <-a.submitCh:
			if current != nil {
				submitReq.resultCh <- fmt.Errorf("session is busy: %s", a.sessionID)
				continue
			}
			task, err := a.startTask(submitReq)
			if err != nil {
				submitReq.resultCh <- err
				continue
			}
			current = task

		case event := <-a.eventCh:
			if current == nil {
				continue
			}
			a.applyTaskEvent(current, event)
			a.maybeFlushTask(current, false)
			a.maybeCompleteTask(current)
			if current.done {
				current = nil
			}

		case <-ticker.C:
			if current == nil {
				continue
			}
			if current.state != nil && current.state.ctx != nil && current.state.ctx.Err() != nil {
				a.finishTask(current, current.state.ctx.Err())
				current.req.resultCh <- nil
				current = nil
				continue
			}

			a.maybeReconcileDuringRun(current)
			a.maybeReconcileOnIdle(current)
			a.maybeFlushTask(current, false)
			a.maybeCompleteTask(current)
			if current.done {
				current = nil
				continue
			}

			if time.Now().After(current.deadline) {
				err := fmt.Errorf("timed out waiting for session %s to complete", a.sessionID)
				a.finishTask(current, err)
				current.req.resultCh <- err
				current = nil
			}
		}
	}
}

func (a *sessionActor) startTask(req *actorSubmitRequest) (*actorRunningTask, error) {
	startedAt := time.Now()
	taskCtx, taskCancel := context.WithCancel(a.runtime.ctx)
	requestTraceID := req.task.RequestTraceID

	initialMessagesCtx, cancelInitial := context.WithTimeout(taskCtx, 4*time.Second)
	initialMessages, initialErr := a.bot.opencodeClient.GetMessages(initialMessagesCtx, a.sessionID)
	cancelInitial()
	if initialErr != nil {
		taskCancel()
		return nil, fmt.Errorf("failed to snapshot session messages before prompt: %w", initialErr)
	}

	initialIDs := make(map[string]bool, len(initialMessages))
	initialDigests := make(map[string]string, len(initialMessages))
	for _, msg := range initialMessages {
		if msg.ID == "" {
			continue
		}
		initialIDs[msg.ID] = true
		initialDigests[msg.ID] = snapshotMessageDigest(msg)
	}

	sendReq := &opencode.SendMessageRequest{
		Parts: []opencode.MessagePart{
			{
				Type: "text",
				Text: req.task.Text,
			},
		},
	}
	if req.task.Model != nil {
		sendReq.Model = req.task.Model
	}

	modelLabel := "unknown"
	if req.task.Model != nil {
		modelLabel = req.task.Model.ProviderID + "/" + req.task.Model.ModelID
	}
	if a.bot != nil && a.bot.config != nil && a.bot.config.Logging.EnableOpenCodeRequestLogs {
		log.Infof("Dispatching OpenCode message: session=%s request_trace_id=%s request_message_id=auto model=%s text_len=%d", a.sessionID, requestTraceID, modelLabel, len(req.task.Text))
	}

	sendTimeout := time.Duration(a.bot.config.OpenCode.Timeout) * time.Second
	if sendTimeout < 8*time.Second {
		sendTimeout = 8 * time.Second
	}
	sendCtx, cancelSend := context.WithTimeout(taskCtx, sendTimeout)
	sendErr := a.bot.opencodeClient.PromptAsync(sendCtx, a.sessionID, sendReq)
	cancelSend()
	if sendErr != nil {
		taskCancel()
		return nil, fmt.Errorf("failed to dispatch prompt_async: %w", sendErr)
	}
	if a.bot != nil && a.bot.config != nil && a.bot.config.Logging.EnableOpenCodeRequestLogs {
		log.Infof("OpenCode prompt_async acknowledged for session %s request_trace_id=%s", a.sessionID, requestTraceID)
	}

	processingMsg, err := a.bot.sendRenderedTelegramMessage(req.task.TelegramCtx, "🤖 Processing...", true)
	if err != nil {
		taskCancel()
		return nil, fmt.Errorf("failed to send processing message: %w", err)
	}

	state := &streamingState{
		ctx:                   taskCtx,
		cancel:                taskCancel,
		done:                  make(chan struct{}),
		telegramMsg:           processingMsg,
		telegramMessages:      []*telebot.Message{processingMsg},
		lastRendered:          []string{"🤖 Processing..."},
		telegramCtx:           req.task.TelegramCtx,
		content:               &strings.Builder{},
		lastUpdate:            time.Now(),
		updateMutex:           &sync.Mutex{},
		sessionID:             a.sessionID,
		requestMessageID:      "",
		requestTraceID:        requestTraceID,
		requestText:           req.task.Text,
		requestStartedAt:      startedAt.UnixMilli(),
		initialMessageIDs:     initialIDs,
		initialMessageDigests: initialDigests,
		eventMessages:         make(map[string]*eventMessageState),
		displaySet:            make(map[string]bool),
		pendingSet:            make(map[string]bool),
		pendingEventParts:     make(map[string][]pendingEventPart),
		lastEventAt:           startedAt,
		sessionStatus:         "busy",
		isStreaming:           true,
	}

	a.bot.streamingStateMu.Lock()
	a.bot.streamingStates[a.sessionID] = state
	a.bot.streamingStateMu.Unlock()

	return &actorRunningTask{
		req:       req,
		state:     state,
		startedAt: startedAt,
		deadline:  startedAt.Add(taskWaitTimeout(a.bot.config.OpenCode.Timeout)),
	}, nil
}

func (a *sessionActor) applyTaskEvent(task *actorRunningTask, event opencode.SessionEvent) {
	if task == nil || task.state == nil {
		return
	}

	task.state.updateMutex.Lock()
	changed, forceFlush := a.bot.applySessionEventLocked(task.state, a.sessionID, event)
	if changed {
		task.state.hasEventUpdates = true
		task.state.lastEventAt = time.Now()
		task.state.revision++
		for a.bot.tryPromoteNextActiveMessage(task.state) {
		}
	}
	task.state.updateMutex.Unlock()

	if forceFlush {
		a.maybeFlushTask(task, true)
	}
}

func (a *sessionActor) maybeReconcileDuringRun(task *actorRunningTask) {
	if task == nil || task.state == nil {
		return
	}

	task.state.updateMutex.Lock()
	needReconcile := task.state.requestObserved && (len(task.state.displayOrder) == 0 || len(task.state.pendingEventParts) > 0)
	task.state.updateMutex.Unlock()
	if !needReconcile {
		return
	}

	performed, _ := a.bot.tryReconcileEventStateWithLatestMessages(task.state, runtimeReconcileInterval, false, "actor-running-reconcile")
	if performed {
		a.maybeFlushTask(task, true)
	}
}

func (a *sessionActor) maybeReconcileOnIdle(task *actorRunningTask) {
	if task == nil || task.state == nil || task.idleReconciled {
		return
	}

	task.state.updateMutex.Lock()
	idle := task.state.sessionStatus == "idle"
	task.state.updateMutex.Unlock()
	if !idle {
		return
	}

	task.idleReconciled = true
	performed, _ := a.bot.tryReconcileEventStateWithLatestMessages(task.state, 0, true, "actor-idle-reconcile")
	if performed {
		a.maybeFlushTask(task, true)
	}
}

func (a *sessionActor) maybeFlushTask(task *actorRunningTask, force bool) {
	if task == nil || task.state == nil {
		return
	}

	var displays []string

	task.state.updateMutex.Lock()
	if !force && time.Since(task.state.lastUpdate) < runtimeFlushInterval {
		task.state.updateMutex.Unlock()
		return
	}
	displays = a.bot.buildEventDrivenDisplaysLocked(task.state)
	if len(displays) > 0 {
		task.state.lastUpdate = time.Now()
	}
	task.state.updateMutex.Unlock()

	if len(displays) == 0 {
		return
	}
	a.bot.updateStreamingTelegramMessages(task.state, displays)
}

func (a *sessionActor) maybeCompleteTask(task *actorRunningTask) {
	if task == nil || task.state == nil || task.done {
		return
	}

	complete, reason := shouldCompleteTask(task)
	if !complete {
		return
	}

	log.Infof("Session actor task completed: session=%s request_trace_id=%s request_message_id=%s reason=%s", a.sessionID, task.state.requestTraceID, task.state.requestMessageID, reason)
	a.finishTask(task, nil)
	task.req.resultCh <- nil
	task.done = true
}

func shouldCompleteTask(task *actorRunningTask) (bool, string) {
	state := task.state
	now := time.Now()

	state.updateMutex.Lock()
	defer state.updateMutex.Unlock()

	if state.sessionStatus != "idle" {
		return false, ""
	}
	if !state.sawIdleAfterBusy {
		return false, ""
	}

	if now.Sub(state.lastEventAt) < runtimeSettleGrace {
		return false, ""
	}
	if len(state.pendingEventParts) > 0 {
		return false, ""
	}

	if len(state.displayOrder) == 0 {
		if !state.requestObserved {
			return false, ""
		}
		if now.Sub(task.startedAt) >= runtimeNoOutputGrace {
			return true, "idle_no_output_grace_elapsed"
		}
		return false, ""
	}

	if allDisplayedMessagesCompletedLocked(state) {
		return true, "idle_completed_messages"
	}

	if now.Sub(state.lastEventAt) >= 3*runtimeSettleGrace {
		return true, "idle_settled_without_completion_markers"
	}

	return false, ""
}

func allDisplayedMessagesCompletedLocked(state *streamingState) bool {
	if state == nil {
		return false
	}
	for _, messageID := range state.displayOrder {
		if !isEventMessageCompleted(state.eventMessages[messageID]) {
			return false
		}
	}
	return true
}

func (a *sessionActor) finishTask(task *actorRunningTask, taskErr error) {
	if task == nil || task.state == nil {
		return
	}
	state := task.state

	var finalDisplays []string

	state.updateMutex.Lock()
	state.isComplete = true
	finalDisplays = a.bot.buildEventDrivenDisplaysLocked(state)
	state.updateMutex.Unlock()

	switch {
	case taskErr != nil && errors.Is(taskErr, context.Canceled):
		// Task was canceled by user (/abort) or shutdown; keep current content as-is.
	case taskErr != nil:
		if state.telegramMsg != nil {
			a.bot.updateTelegramMessage(state.telegramCtx, state.telegramMsg, fmt.Sprintf("Processing error: %v", taskErr), false)
		}
	case len(finalDisplays) > 0:
		a.bot.updateStreamingTelegramMessages(state, finalDisplays)
	default:
		if state.telegramMsg != nil {
			a.bot.updateTelegramMessage(state.telegramCtx, state.telegramMsg, "🤖 Response completed with no content.", false)
		}
	}

	if state.cancel != nil {
		state.cancel()
	}
	if state.done != nil {
		safeCloseStreamDone(state.done)
	}

	state.isStreaming = false

	a.bot.streamingStateMu.Lock()
	current, exists := a.bot.streamingStates[a.sessionID]
	if exists && current == state {
		delete(a.bot.streamingStates, a.sessionID)
	}
	a.bot.streamingStateMu.Unlock()
}

func safeCloseStreamDone(done chan struct{}) {
	defer func() {
		_ = recover()
	}()
	close(done)
}

func taskWaitTimeout(openCodeTimeoutSeconds int) time.Duration {
	waitTimeout := 2 * time.Duration(openCodeTimeoutSeconds) * time.Second
	if waitTimeout < 90*time.Second {
		waitTimeout = 90 * time.Second
	}
	if waitTimeout > 30*time.Minute {
		waitTimeout = 30 * time.Minute
	}
	return waitTimeout
}

func sessionEventSessionIDAndStatus(event opencode.SessionEvent) (sessionID string, status *opencode.SessionStatusInfo) {
	switch event.Type {
	case "message.updated":
		var payload opencode.MessageUpdatedProperties
		if err := json.Unmarshal(event.Properties, &payload); err != nil {
			return "", nil
		}
		return payload.Info.SessionID, nil
	case "message.part.updated":
		var payload opencode.MessagePartUpdatedProperties
		if err := json.Unmarshal(event.Properties, &payload); err != nil {
			return "", nil
		}
		return payload.Part.SessionID, nil
	case "session.status":
		var payload struct {
			SessionID string                     `json:"sessionID"`
			Status    opencode.SessionStatusInfo `json:"status"`
		}
		if err := json.Unmarshal(event.Properties, &payload); err != nil {
			return "", nil
		}
		return payload.SessionID, &payload.Status
	default:
		var payload struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(event.Properties, &payload); err != nil {
			return "", nil
		}
		return payload.SessionID, nil
	}
}
