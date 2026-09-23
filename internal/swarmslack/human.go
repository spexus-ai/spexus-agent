package swarmslack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type HumanStore interface {
	Store
	CommitSlackSource(context.Context, swarm.SlackSource) (bool, error)
	CommittedSlackSource(context.Context, string, string, string) (swarm.SlackSource, error)
	SlackSourceExists(context.Context, string, string, string) (bool, error)
	PendingSlackSources(context.Context, string) ([]swarm.SlackSource, error)
	DeferredStopSources(context.Context, string) ([]swarm.SlackSource, error)
	NewerStopSource(context.Context, string, string) (bool, error)
	SettleSlackSource(context.Context, swarm.SlackSource) error
	SlackWatermark(context.Context, string) (string, error)
	AdvanceSlackWatermark(context.Context, string, string) error
	SetRecoveryBarrier(context.Context, string, string) error
	ActiveHumanRequest(context.Context, string) (*swarm.HumanRequestContext, error)
	IngestUrgent(context.Context, string, swarm.InputPayload) (swarm.Receipt, bool, error)
	InterruptOwnerTurn(context.Context, string, string, string) error
	PublishedHumanQuestion(context.Context, string, string) (string, string, error)
}

type ThreadHistoryAPI interface {
	ScanThread(context.Context, string, string, string, func(string, string, string, string, bool) error) (string, error)
	VerifyWorkspace(context.Context, string) error
}

type humanIngress struct {
	bridge          *Bridge
	store           HumanStore
	history         ThreadHistoryAPI
	workspace       string
	mu              sync.Mutex
	stopMu          sync.Mutex
	statusMu        sync.Mutex
	questionUpdates map[string]string
	statusUpdates   map[string]threadStatusMark
}

type threadStatusMark struct {
	value  string
	at     time.Time
	failed bool
}

// RunHuman is the wire-v2 transport. It does not use the legacy Events API,
// whose ACK precedes consumer delivery.
func (b *Bridge) RunHuman(ctx context.Context, source slack.DurableEventSource, workspace string) error {
	store, ok := b.Store.(HumanStore)
	if !ok || source == nil || workspace == "" {
		return errors.New("durable Slack human ingress is not configured")
	}
	history, ok := b.API.(ThreadHistoryAPI)
	if !ok {
		return errors.New("Slack thread history is not configured")
	}
	if err := history.VerifyWorkspace(ctx, workspace); err != nil {
		return err
	}
	h := &humanIngress{bridge: b, store: store, history: history, workspace: workspace, questionUpdates: map[string]string{}, statusUpdates: map[string]threadStatusMark{}}
	defer h.clearThreadStatuses(context.Background())
	for _, f := range b.Features {
		if _, err := store.SlackWatermark(ctx, f.FeatureID); err != nil {
			return err
		}
		if err := store.SetRecoveryBarrier(ctx, f.FeatureID, "slack_catchup"); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer source.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		reconcile := time.NewTicker(30 * time.Second)
		defer reconcile.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				h.mu.Lock()
				for _, f := range b.Features {
					if err := h.processPending(ctx, f, false); err != nil {
						b.log("Slack source retained for retry: %v", err)
					}
				}
				h.mu.Unlock()
				if err := h.syncQuestionMessages(ctx); err != nil {
					b.log("Slack question update pending: %v", err)
				}
				if err := b.DeliverOne(ctx); err != nil {
					b.log("Slack publication pending: %v", err)
				}
			case <-reconcile.C:
				if err := b.Reconcile(ctx); err != nil {
					b.log("Slack publication reconciliation pending: %v", err)
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	err := source.RunDurable(ctx, h.connected, h.disconnected, h.handle)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type questionUpdater interface {
	UpdateHumanQuestion(context.Context, swarm.SlackDelivery, []swarm.HumanOption, string, string) error
}

type threadStatusUpdater interface {
	SetThreadStatus(context.Context, string, string, string) error
}

func (h *humanIngress) clearThreadStatuses(ctx context.Context) {
	api, ok := h.bridge.API.(threadStatusUpdater)
	if !ok {
		return
	}
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	h.statusUpdates = map[string]threadStatusMark{}
	for _, f := range h.bridge.Features {
		if err := api.SetThreadStatus(callCtx, f.ChannelID, f.ThreadTS, ""); err != nil {
			h.bridge.log("Slack typing status clear pending: %v", err)
			h.statusUpdates[f.FeatureID] = threadStatusMark{value: "", at: time.Now(), failed: true}
		}
	}
}

func (h *humanIngress) updateThreadStatus(ctx context.Context, f swarm.Feature, desired string) {
	api, ok := h.bridge.API.(threadStatusUpdater)
	if !ok {
		return
	}
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	if h.statusUpdates == nil {
		h.statusUpdates = map[string]threadStatusMark{}
	}
	old, present := h.statusUpdates[f.FeatureID]
	if desired == "" && (!present || (old.value == "" && !old.failed)) {
		return
	}
	refreshAfter := 90 * time.Second
	if old.failed {
		refreshAfter = 30 * time.Second
	}
	if desired == old.value && time.Since(old.at) < refreshAfter {
		return
	}
	if desired != "" && h.store != nil {
		view, err := h.store.History(ctx, f.FeatureID)
		if err != nil || view.RecoveryBarrier != "" || view.Feature.Stopped {
			desired = ""
			if old.value == "" && !old.failed {
				return
			}
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	err := api.SetThreadStatus(callCtx, f.ChannelID, f.ThreadTS, desired)
	cancel()
	h.statusUpdates[f.FeatureID] = threadStatusMark{value: desired, at: time.Now(), failed: err != nil}
	if err != nil {
		h.bridge.log("Slack typing status unavailable: %v", err)
	}
}

// Legacy text questions are upgraded in place; canonical backend state removes
// buttons after a decision. chat.update is repeatable across coordinator restarts.
func (h *humanIngress) syncQuestionMessages(ctx context.Context) error {
	api, ok := h.bridge.API.(questionUpdater)
	if !ok {
		return errors.New("Slack question update API unavailable")
	}
	for _, f := range h.bridge.Features {
		view, err := h.store.History(ctx, f.FeatureID)
		if err != nil {
			return err
		}
		pending := false
		for _, sent := range view.SlackOutbox {
			if sent.Status != "sent" || sent.SlackTS == "" {
				continue
			}
			for _, projection := range view.HumanRequests {
				if projection.RequestID != sent.ID {
					continue
				}
				state := humanQuestionState(view, sent.ID, projection)
				if state == "pending" {
					pending = true
				}
				if h.questionUpdates[sent.ID] == state {
					break
				}
				options := []swarm.HumanOption{}
				for _, d := range view.Dependencies {
					if d.RequestID == sent.ID {
						options = d.Blocker.Options
						break
					}
				}
				if err := api.UpdateHumanQuestion(ctx, sent, options, state, canonicalDecisionSummary(projection, options)); err != nil {
					return err
				}
				h.questionUpdates[sent.ID] = state
				break
			}
		}
		h.updateThreadStatus(ctx, f, desiredThreadStatus(view, pending))
	}
	return nil
}

func desiredThreadStatus(view swarm.History, decisionPending bool) string {
	if view.Feature.Stopped || view.RecoveryBarrier != "" {
		return ""
	}
	var owner *swarm.AgentStatus
	for _, agent := range view.Agents {
		if agent.AgentID == view.Feature.OwnerAgentID && agent.Status == "online" {
			current := agent
			owner = &current
			break
		}
	}
	if owner != nil {
		for _, turn := range view.Turns {
			if turn.State == "running" {
				if owner.OwnerTurnID == turn.TurnID && turn.TurnID != "" {
					return activityStatus(owner.ActivityPhase, "оркестратор")
				}
				return "готовит ответ…"
			}
		}
	}
	for _, job := range view.Jobs {
		for _, attempt := range job.Attempts {
			if attempt.State != "running" || attempt.AttemptID != job.CurrentAttemptID {
				continue
			}
			for _, agent := range view.Agents {
				if agent.AgentID == attempt.AssignedAgentID && agent.Status == "online" && agent.ActiveAttemptID == attempt.AttemptID {
					return activityStatus(agent.ActivityPhase, "исполнитель")
				}
			}
		}
	}
	if decisionPending {
		return "проверяет ответ…"
	}
	return ""
}

func activityStatus(phase, actor string) string {
	switch phase {
	case "thinking":
		return actor + " думает…"
	case "tool":
		return actor + " использует инструмент…"
	case "responding":
		return actor + " пишет ответ…"
	default:
		return actor + " работает…"
	}
}

func humanQuestionState(view swarm.History, requestID string, projection swarm.HumanProjection) string {
	if projection.BackendState != "open" {
		return projection.BackendState
	}
	if view.Feature.Stopped {
		return "stopped"
	}
	pending, blocked := false, false
	for _, op := range view.HumanSync {
		if op.RequestID != requestID || op.Kind != "decision" {
			continue
		}
		if op.Status == "blocked" {
			blocked = true
		} else {
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	if blocked {
		return "attention"
	}
	return "open"
}

func canonicalDecisionSummary(projection swarm.HumanProjection, options []swarm.HumanOption) string {
	if projection.BackendState != "answered" && projection.BackendState != "denied" {
		return ""
	}
	var view struct {
		Terminal *struct {
			Kind     string `json:"kind"`
			Response struct {
				OptionID *string `json:"option_id"`
				Text     string  `json:"text"`
			} `json:"response"`
		} `json:"terminal"`
	}
	if json.Unmarshal(projection.View, &view) != nil || view.Terminal == nil {
		return ""
	}
	if projection.BackendState == "denied" && view.Terminal.Kind == "deny" && strings.TrimSpace(view.Terminal.Response.Text) != "" {
		return "Отказ с причиной записан."
	}
	if projection.BackendState != "answered" || view.Terminal.Kind != "answer" {
		return ""
	}
	if view.Terminal.Response.OptionID == nil {
		if strings.TrimSpace(view.Terminal.Response.Text) != "" {
			return "Ответ свободным текстом записан."
		}
		return ""
	}
	for _, option := range options {
		if option.ID == *view.Terminal.Response.OptionID {
			return "Выбран вариант: " + option.Label + "."
		}
	}
	return ""
}

func (h *humanIngress) connected(ctx context.Context) error {
	for _, f := range h.bridge.Features {
		if err := h.store.SetRecoveryBarrier(ctx, f.FeatureID, "slack_catchup"); err != nil {
			return err
		}
	}
	h.clearThreadStatuses(ctx)
	go h.catchup(ctx)
	return nil
}

func (h *humanIngress) disconnected(ctx context.Context) error {
	h.mu.Lock()
	for _, f := range h.bridge.Features {
		if err := h.store.SetRecoveryBarrier(ctx, f.FeatureID, "slack_disconnected"); err != nil {
			h.mu.Unlock()
			return err
		}
	}
	h.mu.Unlock()
	h.clearThreadStatuses(ctx)
	return nil
}

func (h *humanIngress) catchup(ctx context.Context) {
	for ctx.Err() == nil {
		err := h.scanAll(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		h.bridge.log("Slack history catchup pending: %v", err)
		delay := 5 * time.Second
		var limited *RateLimitError
		if errors.As(err, &limited) && limited.After > delay {
			delay = limited.After
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (h *humanIngress) scanAll(ctx context.Context) error {
	for _, f := range h.bridge.Features {
		watermark, err := h.store.SlackWatermark(ctx, f.FeatureID)
		if err != nil {
			return err
		}
		upper, err := h.history.ScanThread(ctx, f.ChannelID, f.ThreadTS, watermark, func(ts, actor, text, thread string, edited bool) error {
			if !timestampAfter(ts, watermark) {
				return nil
			}
			if edited {
				exists, err := h.store.SlackSourceExists(ctx, h.workspace, f.ChannelID, ts)
				if err != nil {
					return err
				}
				if exists {
					return fmt.Errorf("edited Slack source conflict at %s", ts)
				}
				return nil
			}
			_, err := h.commit(ctx, f, slack.Event{WorkspaceID: h.workspace, ChannelID: f.ChannelID, ThreadTS: thread, Timestamp: ts, UserID: actor, Text: text})
			return err
		})
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h.mu.Lock()
		// While the barrier remains set, stops are already applied and all other
		// sources run in timestamp order. Historical continue is never replayed.
		err = h.processPending(ctx, f, true)
		// A Socket !stop locks stopMu before its SQLite commit. The final stop
		// drain and barrier release hold the same mutex, so a committed stop
		// cannot be waiting for its latch when work becomes eligible again.
		h.stopMu.Lock()
		if err == nil {
			err = h.processStops(ctx, f.FeatureID)
		}
		if err == nil && upper != "" && timestampAfter(upper, watermark) {
			err = h.store.AdvanceSlackWatermark(ctx, f.FeatureID, upper)
		}
		if err == nil && ctx.Err() == nil {
			err = h.store.SetRecoveryBarrier(ctx, f.FeatureID, "")
		}
		h.stopMu.Unlock()
		h.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *humanIngress) handle(ctx context.Context, event slack.Event) error {
	if event.FeatureControl != nil {
		for _, f := range h.bridge.Features {
			if f.FeatureID == event.FeatureControl.FeatureID && f.ChannelID == event.ChannelID && f.ThreadTS == event.FeatureControl.AnchorTS && event.ThreadTS == f.ThreadTS {
				_, err := h.commit(ctx, f, event)
				return err
			}
		}
		return errors.New("Slack feature control scope mismatch")
	}
	if event.HumanAction != nil {
		featureID, thread, err := h.store.PublishedHumanQuestion(ctx, event.HumanAction.RequestID, event.HumanAction.QuestionTS)
		if err != nil {
			return err
		}
		if event.ThreadTS != "" && event.ThreadTS != thread {
			return errors.New("Slack action thread mismatch")
		}
		event.ThreadTS = thread
		for _, f := range h.bridge.Features {
			if f.FeatureID == featureID && f.ChannelID == event.ChannelID && f.ThreadTS == thread {
				_, err = h.commit(ctx, f, event)
				return err
			}
		}
		return errors.New("Slack action feature mismatch")
	}
	for _, f := range h.bridge.Features {
		if f.ChannelID != event.ChannelID || f.ThreadTS != event.ThreadTimestamp() {
			continue
		}
		_, err := h.commit(ctx, f, event)
		return err
	}
	return nil
}

func (h *humanIngress) commit(ctx context.Context, f swarm.Feature, event slack.Event) (bool, error) {
	if event.WorkspaceID == "" || event.WorkspaceID != h.workspace {
		return false, errors.New("Slack event workspace mismatch")
	}
	if !allowed(f, event.UserID) {
		return false, nil
	}
	if event.FeatureControl != nil {
		if event.FeatureControl.ControlID != "stop" {
			return false, errors.New("unknown feature control")
		}
		event.Text = "!stop"
	}
	if event.HumanAction != nil && event.HumanAction.ControlID != "" {
		switch event.HumanAction.ControlID {
		case "details":
			event.Text = swarm.HumanDetailsControlText
		case "stop":
			event.Text = "!stop"
		default:
			return false, errors.New("unknown human control")
		}
	}
	stop := event.Text == "!stop"
	if stop {
		h.stopMu.Lock()
		defer h.stopMu.Unlock()
	}
	in := swarm.SlackSource{WorkspaceID: h.workspace, ChannelID: event.ChannelID, MessageTS: event.Timestamp, ThreadTS: event.ThreadTimestamp(), FeatureID: f.FeatureID, ActorID: event.UserID, Text: event.Text, EventID: event.ID}
	if event.HumanAction != nil {
		in.SourceKind, in.QuestionTS, in.RequestID = "block_action", event.HumanAction.QuestionTS, event.HumanAction.RequestID
		if event.HumanAction.ControlID != "" {
			in.SourceKind, in.OptionID = "button_control", event.HumanAction.ControlID
		} else {
			in.OptionID = event.HumanAction.OptionID
		}
	}
	if event.FeatureControl != nil {
		in.SourceKind, in.QuestionTS, in.OptionID = "feature_control", event.FeatureControl.AnchorTS, event.FeatureControl.ControlID
	}
	duplicate, err := h.store.CommitSlackSource(ctx, in)
	if err != nil {
		return false, err
	}
	if stop {
		err = h.processStop(ctx, in)
		if err != nil {
			// A committed stop with no applied latch must block new starts.
			_ = h.store.SetRecoveryBarrier(ctx, f.FeatureID, "slack_stop_pending")
			return duplicate, err
		}
		return duplicate, nil
	}
	urgent := strings.HasPrefix(in.Text, "!")
	if urgent && in.Text == "!continue" {
		view, err := h.store.History(ctx, in.FeatureID)
		if err != nil {
			return duplicate, err
		}
		urgent = !view.Feature.Stopped
	}
	if urgent {
		in, err = h.store.CommittedSlackSource(ctx, in.WorkspaceID, in.ChannelID, in.MessageTS)
		if err != nil {
			return duplicate, err
		}
		input, err := h.ownerInput(ctx, in)
		if err != nil {
			return duplicate, err
		}
		if _, _, err = h.store.IngestUrgent(ctx, in.FeatureID, input); err != nil {
			return duplicate, err
		}
		if err = h.store.InterruptOwnerTurn(ctx, in.FeatureID, in.ActorID, "urgent Slack message"); err != nil {
			return duplicate, err
		}
		if in.Text == "!continue" {
			return duplicate, h.store.SettleSlackSource(ctx, in)
		}
	}
	// Normal commands are reduced from the local queue after the Socket ACK.
	// No backend, model, history, or Slack publish call extends the ACK path.
	return duplicate, nil
}

func allowed(f swarm.Feature, actor string) bool {
	for _, id := range f.AllowedActorIDs {
		if actor == id {
			return true
		}
	}
	return false
}

func (h *humanIngress) processStop(ctx context.Context, source swarm.SlackSource) error {
	pending, err := h.store.PendingSlackSources(ctx, source.FeatureID)
	if err != nil {
		return err
	}
	for _, in := range pending {
		if in.ChannelID == source.ChannelID && in.MessageTS == source.MessageTS {
			view, err := h.store.History(ctx, in.FeatureID)
			if err != nil {
				return err
			}
			if !view.Feature.Stopped {
				if err := h.store.StopFeature(ctx, in.FeatureID, in.ActorID, "Slack !stop"); err != nil {
					return err
				}
			}
			if err := h.notice(ctx, in, "Работа остановлена. Активные процессы завершаются штатным контролем."); err != nil {
				return err
			}
			return h.store.SettleSlackSource(ctx, in)
		}
	}
	return nil
}

func (h *humanIngress) processPending(ctx context.Context, f swarm.Feature, recovering bool) error {
	if !recovering {
		view, err := h.store.History(ctx, f.FeatureID)
		if err != nil {
			return err
		}
		if view.RecoveryBarrier != "" {
			return nil
		}
	}
	h.stopMu.Lock()
	err := h.processStops(ctx, f.FeatureID)
	h.stopMu.Unlock()
	if err != nil {
		return err
	}
	pending, err := h.store.PendingSlackSources(ctx, f.FeatureID)
	if err != nil {
		return err
	}
	view, err := h.store.History(ctx, f.FeatureID)
	if err != nil {
		return err
	}
	if view.Feature.Stopped {
		// Keep ordinary human messages durable while stopped. A later explicit
		// continue reopens the feature and then drains them; historical continue
		// during catchup is acknowledged without replaying the old command.
		for _, in := range pending {
			if in.Text == "!continue" {
				if err := h.processOne(ctx, in, recovering); err != nil {
					return err
				}
			}
		}
		view, err = h.store.History(ctx, f.FeatureID)
		if err != nil || view.Feature.Stopped {
			return err
		}
		pending, err = h.store.PendingSlackSources(ctx, f.FeatureID)
		if err != nil {
			return err
		}
	}
	sort.Slice(pending, func(i, j int) bool { return timestampAfter(pending[j].MessageTS, pending[i].MessageTS) })
	for _, in := range pending {
		if in.Text == "!stop" {
			continue
		}
		if err := h.processOne(ctx, in, recovering); err != nil {
			return err
		}
	}
	return nil
}

// processStops runs while stopMu is held. Other commands never clear a stop
// during catchup; the caller drains it again immediately before release.
func (h *humanIngress) processStops(ctx context.Context, featureID string) error {
	pending, err := h.store.PendingSlackSources(ctx, featureID)
	if err != nil {
		return err
	}
	for _, in := range pending {
		if in.Text == "!stop" {
			if err := h.processStop(ctx, in); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *humanIngress) processOne(ctx context.Context, in swarm.SlackSource, recovering bool) error {
	if in.Text == "!continue" {
		if recovering || in.DuringCatchup {
			if err := h.notice(ctx, in, "После сверки истории отправьте новую команду !continue, если хотите продолжить работу."); err != nil {
				return err
			}
			return h.store.SettleSlackSource(ctx, in)
		}
		h.stopMu.Lock()
		newerStop, err := h.store.NewerStopSource(ctx, in.FeatureID, in.MessageTS)
		if err != nil {
			h.stopMu.Unlock()
			return err
		}
		if newerStop {
			h.stopMu.Unlock()
			return h.store.SettleSlackSource(ctx, in)
		}
		view, err := h.store.History(ctx, in.FeatureID)
		if err != nil {
			h.stopMu.Unlock()
			return err
		}
		if view.Feature.Stopped {
			if err := h.store.ContinueFeature(ctx, in.FeatureID, in.ActorID); err != nil {
				h.stopMu.Unlock()
				return h.rejection(ctx, in, err)
			}
		}
		h.stopMu.Unlock()
		stops, err := h.store.DeferredStopSources(ctx, in.FeatureID)
		if err != nil {
			return err
		}
		for _, stop := range stops {
			if timestampAfter(stop.MessageTS, in.MessageTS) {
				continue
			}
			stopInput, err := h.ownerInput(ctx, stop)
			if err != nil {
				return err
			}
			if _, _, err = h.store.IngestUrgent(ctx, stop.FeatureID, stopInput); err != nil {
				return err
			}
		}
	}
	input, err := h.ownerInput(ctx, in)
	if err != nil {
		return err
	}
	if strings.HasPrefix(in.Text, "!") {
		_, _, err = h.store.IngestUrgent(ctx, in.FeatureID, input)
		if err == nil {
			err = h.store.InterruptOwnerTurn(ctx, in.FeatureID, in.ActorID, "urgent Slack message")
		}
	} else {
		_, _, err = h.store.Ingest(ctx, in.FeatureID, input)
	}
	if err != nil {
		var api *swarm.APIError
		if !errors.As(err, &api) || api.Status >= 500 {
			return err
		}
		return h.rejection(ctx, in, err)
	}
	return h.store.SettleSlackSource(ctx, in)
}

func (h *humanIngress) ownerInput(ctx context.Context, in swarm.SlackSource) (swarm.InputPayload, error) {
	input := swarm.InputPayload{Text: in.Text, Source: swarm.Source{Kind: "slack", EventID: "slack:" + in.ChannelID + ":" + in.MessageTS, ChannelID: in.ChannelID, ThreadTS: in.ThreadTS, ActorID: in.ActorID, MessageTS: in.MessageTS}, ActiveHumanRequest: in.ActiveHumanRequest}
	if in.SourceKind == "block_action" {
		input.HumanAction = &swarm.HumanActionInput{RequestID: in.RequestID, OptionID: in.OptionID}
		input.Text = "Выбран вариант кнопкой в Slack."
	}
	return input, nil
}

func (h *humanIngress) rejection(ctx context.Context, in swarm.SlackSource, reason error) error {
	code := "invalid_request"
	var api *swarm.APIError
	if errors.As(reason, &api) {
		code = api.Code
	}
	message := "Сообщение пока не обработано: " + code + ". Попробуйте ещё раз или сообщите о проблеме в этом треде."
	if in.SourceKind == "block_action" {
		message = "Выбор пока не обработан: " + code + ". Попробуйте ещё раз или сообщите о проблеме в этом треде."
	}
	if err := h.notice(ctx, in, message); err != nil {
		return err
	}
	return h.store.SettleSlackSource(ctx, in)
}

func (h *humanIngress) notice(ctx context.Context, in swarm.SlackSource, text string) error {
	err := h.store.QueueSlackNotice(ctx, in.FeatureID, "source:"+in.ChannelID+":"+in.MessageTS, text)
	var api *swarm.APIError
	if errors.As(err, &api) && api.Code == "idempotency_conflict" {
		// The first response is already durable; a changed status view after a
		// crash must not create another Slack publication.
		return nil
	}
	return err
}
