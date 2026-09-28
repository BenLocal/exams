// Package notify delivers notifications about newly discovered exam
// announcements.
//
// Delivery is claimed in the database before it is attempted, so a crash
// mid-send cannot lose a notification, and a failed send is retried on the
// next run rather than being suppressed forever.
package notify

import (
	"context"
	"log/slog"
	"time"
)

// Event describes one announcement worth telling someone about.
type Event struct {
	ExamID      int64
	SourceKey   string
	SourceName  string
	Action      string // model.ActionCreated or model.ActionUpdated
	Title       string
	URL         string
	Summary     string
	Category    string
	Region      string
	PublishedAt *time.Time
	DeadlineAt  *time.Time
}

// Target is one delivery destination: a channel plus its address.
type Target struct {
	Channel string // "email" or "webhook"
	Address string // email address or webhook URL
}

// Notifier delivers events to its targets. Implementations must be safe for
// concurrent use.
type Notifier interface {
	// Targets lists the destinations this notifier serves. Each is claimed
	// individually so one bad address cannot block the others.
	Targets() []Target
	// Send delivers a batch to one target. Events are always sent as a batch:
	// the chat platforms rate limit hard, and one message listing ten
	// announcements is both cheaper and more readable than ten messages.
	Send(ctx context.Context, t Target, events []Event) error
}

// ClaimStore is the subset of the store the dispatcher needs.
type ClaimStore interface {
	ClaimNotification(ctx context.Context, examID int64, channel, target, event string) (int64, bool, error)
	RecordNotificationResult(ctx context.Context, id int64, sent bool, errMsg string) error
}

// Dispatcher fans events out to every notifier and target.
type Dispatcher struct {
	notifiers []Notifier
	store     ClaimStore
	log       *slog.Logger
}

// NewDispatcher builds a dispatcher. Notifiers with no targets are dropped.
func NewDispatcher(st ClaimStore, log *slog.Logger, notifiers ...Notifier) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	kept := make([]Notifier, 0, len(notifiers))
	for _, n := range notifiers {
		if n != nil && len(n.Targets()) > 0 {
			kept = append(kept, n)
		}
	}
	return &Dispatcher{notifiers: kept, store: st, log: log}
}

// Enabled reports whether anything would actually be delivered.
func (d *Dispatcher) Enabled() bool {
	return d != nil && len(d.notifiers) > 0
}

// Dispatch delivers events to every configured target.
//
// It never returns an error: notifications are best-effort by design, and a
// failing chat webhook must not fail an otherwise successful crawl. Per-target
// failures are recorded and surfaced on the runs page.
func (d *Dispatcher) Dispatch(ctx context.Context, events []Event) {
	if d == nil || len(events) == 0 {
		return
	}
	for _, n := range d.notifiers {
		for _, t := range n.Targets() {
			d.dispatchOne(ctx, n, t, events)
		}
	}
}

func (d *Dispatcher) dispatchOne(ctx context.Context, n Notifier, t Target, events []Event) {
	claimed := make([]Event, 0, len(events))
	ids := make([]int64, 0, len(events))

	for _, ev := range events {
		id, ok, err := d.store.ClaimNotification(ctx, ev.ExamID, t.Channel, t.Address, ev.Action)
		if err != nil {
			d.log.Error("claim notification failed", "channel", t.Channel, "target", t.Address, "err", err)
			continue
		}
		if !ok {
			continue // already delivered
		}
		claimed = append(claimed, ev)
		ids = append(ids, id)
	}
	if len(claimed) == 0 {
		return
	}

	sendErr := n.Send(ctx, t, claimed)
	errMsg := ""
	if sendErr != nil {
		errMsg = truncate(sendErr.Error(), 500)
		d.log.Warn("notification delivery failed",
			"channel", t.Channel, "target", t.Address, "count", len(claimed), "err", sendErr)
	} else {
		d.log.Info("notification delivered",
			"channel", t.Channel, "target", t.Address, "count", len(claimed))
	}

	for _, id := range ids {
		if err := d.store.RecordNotificationResult(ctx, id, sendErr == nil, errMsg); err != nil {
			d.log.Error("record notification result failed", "id", id, "err", err)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
