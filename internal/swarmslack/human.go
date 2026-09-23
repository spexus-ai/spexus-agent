package swarmslack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	SlackSourceExists(context.Context, string, string, string) (bool, error)
	PendingSlackSources(context.Context, string) ([]swarm.SlackSource, error)
	SettleSlackSource(context.Context, swarm.SlackSource) error
	SlackWatermark(context.Context, string) (string, error)
	AdvanceSlackWatermark(context.Context, string, string) error
	SetRecoveryBarrier(context.Context, string, string) error
	RecordHumanAnswer(context.Context, swarm.HumanAnswerInput) (string, bool, error)
}

type ThreadHistoryAPI interface {
	ScanThread(context.Context, string, string, string, func(string, string, string, string, bool) error) (string, error)
	VerifyWorkspace(context.Context, string) error
}

type humanIngress struct {
	bridge    *Bridge
	store     HumanStore
	history   ThreadHistoryAPI
	workspace string
	mu        sync.Mutex
	stopMu    sync.Mutex
}

var requestIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

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
	h := &humanIngress{bridge: b, store: store, history: history, workspace: workspace}
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

func (h *humanIngress) connected(ctx context.Context) error {
	for _, f := range h.bridge.Features {
		if err := h.store.SetRecoveryBarrier(ctx, f.FeatureID, "slack_catchup"); err != nil {
			return err
		}
	}
	go h.catchup(ctx)
	return nil
}

func (h *humanIngress) disconnected(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.bridge.Features {
		if err := h.store.SetRecoveryBarrier(ctx, f.FeatureID, "slack_disconnected"); err != nil {
			return err
		}
	}
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
	stop := strings.TrimSpace(event.Text) == "!stop"
	if stop {
		h.stopMu.Lock()
		defer h.stopMu.Unlock()
	}
	in := swarm.SlackSource{WorkspaceID: h.workspace, ChannelID: event.ChannelID, MessageTS: event.Timestamp, ThreadTS: event.ThreadTimestamp(), FeatureID: f.FeatureID, ActorID: event.UserID, Text: event.Text, EventID: event.ID}
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
			if err := h.store.StopFeature(ctx, in.FeatureID, in.ActorID, "Slack !stop"); err != nil {
				return err
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
	sort.Slice(pending, func(i, j int) bool { return timestampAfter(pending[j].MessageTS, pending[i].MessageTS) })
	for _, in := range pending {
		if strings.TrimSpace(in.Text) == "!stop" {
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
		if strings.TrimSpace(in.Text) == "!stop" {
			if err := h.processStop(ctx, in); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *humanIngress) processOne(ctx context.Context, in swarm.SlackSource, recovering bool) error {
	text := strings.TrimSpace(in.Text)
	switch {
	case text == "!continue":
		if recovering || in.DuringCatchup {
			if err := h.notice(ctx, in, "После сверки истории отправьте новую команду !continue, если хотите продолжить работу."); err != nil {
				return err
			}
		} else {
			if err := h.store.ContinueFeature(ctx, in.FeatureID, in.ActorID); err != nil {
				return h.rejection(ctx, in, err)
			}
			if err := h.notice(ctx, in, "Работа открыта для новых команд. Отменённые зависимости не возобновляются."); err != nil {
				return err
			}
		}
	case text == "!status":
		view, err := h.store.History(ctx, in.FeatureID)
		if err != nil {
			return err
		}
		if err := h.notice(ctx, in, humanStatus(view)); err != nil {
			return err
		}
	case strings.HasPrefix(text, "!answer"):
		answer, err := parseAnswer(text)
		if err != nil {
			return h.rejection(ctx, in, err)
		}
		answer.WorkspaceID, answer.ChannelID, answer.ThreadTS = in.WorkspaceID, in.ChannelID, in.ThreadTS
		answer.MessageTS, answer.ActorID = in.MessageTS, in.ActorID
		answer.EventID = "" // Socket and history share the same canonical source.
		if _, _, err := h.store.RecordHumanAnswer(ctx, answer); err != nil {
			var api *swarm.APIError
			if errors.As(err, &api) && api.Code == "request_not_waiting" {
				view, viewErr := h.store.History(ctx, in.FeatureID)
				if viewErr != nil {
					return viewErr
				}
				for _, d := range view.Dependencies {
					if d.RequestID == answer.RequestID && d.State == "human_pending" {
						return err // Create/readback is still pending; retain the source.
					}
				}
			}
			if !errors.As(err, &api) || api.Status >= 500 {
				return err
			}
			return h.rejection(ctx, in, err)
		}
		if err := h.notice(ctx, in, "Ответ сохранён локально. Ожидаем подтверждение решения в Spexus; затем оркестратор продолжит связанную работу."); err != nil {
			return err
		}
	default:
		_, _, err := h.store.Ingest(ctx, in.FeatureID, swarm.InputPayload{Text: in.Text, Source: swarm.Source{Kind: "slack", EventID: "slack:" + in.ChannelID + ":" + in.MessageTS, ChannelID: in.ChannelID, ThreadTS: in.ThreadTS, ActorID: in.ActorID}})
		if err != nil {
			var api *swarm.APIError
			if !errors.As(err, &api) || api.Status >= 500 {
				return err
			}
			return h.rejection(ctx, in, err)
		}
	}
	return h.store.SettleSlackSource(ctx, in)
}

func (h *humanIngress) rejection(ctx context.Context, in swarm.SlackSource, reason error) error {
	code := "invalid_request"
	var api *swarm.APIError
	if errors.As(reason, &api) {
		code = api.Code
	}
	if err := h.notice(ctx, in, "Запрос не принят: "+code+". Проверьте полный UUID и используйте !status."); err != nil {
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

func parseAnswer(text string) (swarm.HumanAnswerInput, error) {
	fields := strings.Fields(text)
	if len(fields) < 3 || fields[0] != "!answer" || !requestIDPattern.MatchString(fields[1]) {
		return swarm.HumanAnswerInput{}, errors.New("invalid answer syntax")
	}
	in := swarm.HumanAnswerInput{RequestID: strings.ToLower(fields[1]), Kind: "answer"}
	switch fields[2] {
	case "text":
		if len(fields) < 4 {
			return in, errors.New("answer text required")
		}
		in.Text = strings.Join(fields[3:], " ")
	case "deny":
		if len(fields) < 4 {
			return in, errors.New("deny reason required")
		}
		in.Kind = "deny"
		in.Text = strings.Join(fields[3:], " ")
	default:
		if fields[2] == "" || len(fields[2]) > 64 {
			return in, errors.New("invalid option ID")
		}
		for _, r := range fields[2] {
			if r > 127 || r <= 32 {
				return in, errors.New("invalid option ID")
			}
		}
		in.OptionID = fields[2]
		if len(fields) > 3 {
			in.Text = strings.Join(fields[3:], " ")
		}
	}
	if len(in.Text) > 16*1024 {
		return in, errors.New("answer too long")
	}
	return in, nil
}

func humanStatus(v swarm.History) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Фича %s · владелец %s · остановлена=%t", v.Feature.FeatureID, v.Feature.OwnerAgentID, v.Feature.Stopped)
	if v.RecoveryBarrier != "" {
		fmt.Fprintf(&b, "\nСверка Slack: %s; новые запуски ожидают завершения.", v.RecoveryBarrier)
	}
	for _, d := range v.Dependencies {
		fmt.Fprintf(&b, "\nЗависимость %s · %s · запрос %s · job %s · attempt %s", d.ID, d.State, d.RequestID, d.JobID, d.AttemptID)
	}
	for _, p := range v.HumanRequests {
		fmt.Fprintf(&b, "\nРешение %s · backend %s rev%d · применение %s", p.RequestID, p.BackendState, p.Revision, p.ApplicationStatus)
	}
	for _, s := range v.HumanSync {
		if s.Status != "done" {
			fmt.Fprintf(&b, "\nСинхронизация %s · %s · %s", s.RequestID, s.Kind, s.Status)
		}
	}
	if b.Len() <= 16*1024 {
		return b.String()
	}
	var truncated strings.Builder
	for _, r := range b.String() {
		if truncated.Len()+len(string(r))+len("\n… список сокращён") > 16*1024 {
			break
		}
		truncated.WriteRune(r)
	}
	truncated.WriteString("\n… список сокращён")
	return truncated.String()
}
