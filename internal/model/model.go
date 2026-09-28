// Package model holds the domain types shared by the store, collectors,
// scheduler and web layers.
package model

import "time"

// Source is a configured crawl target. Rows are upserted from the collector
// registry at startup, so adding a collector in Go is enough to make it appear
// here. Operator choices (enabled, interval) are never overwritten by that
// sync.
type Source struct {
	Key           string     `json:"key"`
	Name          string     `json:"name"`
	BaseURL       string     `json:"base_url"`
	Enabled       bool       `json:"enabled"`
	Interval      string     `json:"interval"`
	BootstrapDone bool       `json:"bootstrap_done"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Item is a single scraped announcement before it is written to the database.
// It is the hand-off type between collectors and the store.
type Item struct {
	ExternalID  string
	Title       string
	URL         string
	Summary     string
	Content     string
	Category    string
	Region      string
	PublishedAt *time.Time
	// PublishedRaw preserves the source's own date text so an ambiguous parse
	// never loses the original.
	PublishedRaw string
	DeadlineAt   *time.Time
}

// Exam is a stored announcement.
type Exam struct {
	ID           int64      `json:"id"`
	SourceKey    string     `json:"source_key"`
	ExternalID   string     `json:"external_id"`
	Title        string     `json:"title"`
	URL          string     `json:"url"`
	Summary      string     `json:"summary"`
	Content      string     `json:"content"`
	Category     string     `json:"category"`
	Region       string     `json:"region"`
	PublishedAt  *time.Time `json:"published_at,omitempty"`
	PublishedRaw string     `json:"published_raw,omitempty"`
	DeadlineAt   *time.Time `json:"deadline_at,omitempty"`
	ListHash     string     `json:"-"`
	BodyHash     string     `json:"-"`
	IsRead       bool       `json:"is_read"`
	FirstSeenAt  time.Time  `json:"first_seen_at"`
	LastSeenAt   time.Time  `json:"last_seen_at"`
	UpdatedAt    time.Time  `json:"updated_at"`

	// SourceName is joined in for display; empty on writes.
	SourceName string `json:"source_name,omitempty"`
}

// Change actions recorded in exam_changes.
const (
	ActionCreated   = "created"
	ActionUpdated   = "updated"
	ActionUnchanged = "unchanged"
)

// Crawl run statuses. StatusEmpty means the fetch worked but produced no
// items, which almost always indicates a broken selector rather than a quiet
// news day.
const (
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusEmpty   = "empty"
	StatusFailed  = "failed"
)

// CrawlRun records one execution of one collector. Every run is recorded,
// including failures — this table is the answer to "why is there no data?".
type CrawlRun struct {
	ID         int64      `json:"id"`
	SourceKey  string     `json:"source_key"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Fetched    int        `json:"fetched"`
	Inserted   int        `json:"inserted"`
	Updated    int        `json:"updated"`
	Unchanged  int        `json:"unchanged"`
	Error      string     `json:"error"`
	Note       string     `json:"note"`

	// Joined in for display.
	SourceName string `json:"source_name,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// ExamChange is an audit row written whenever a crawl creates or modifies an
// exam. ChangedFields is empty for creations.
type ExamChange struct {
	ID            int64     `json:"id"`
	ExamID        int64     `json:"exam_id"`
	RunID         *int64    `json:"run_id,omitempty"`
	ChangeType    string    `json:"change_type"`
	ChangedFields []string  `json:"changed_fields"`
	CreatedAt     time.Time `json:"created_at"`
}

// UpsertResult reports what a single item did during an upsert.
type UpsertResult struct {
	Action        string
	ExamID        int64
	ChangedFields []string
}

// UpsertOutcome summarises what a batch of items did.
type UpsertOutcome struct {
	Inserted  int
	Updated   int
	Unchanged int
	// Created and Changed list the affected exams in processing order.
	// Notifications are driven from these, and they are kept separate because
	// most operators want to hear about new announcements but not about
	// routine edits to ones they have already seen.
	Created []Exam
	Changed []Exam
}

// ItemFilter drives the list page and the JSON API. The zero value means
// "everything, newest first, page 1".
type ItemFilter struct {
	Query    string
	Source   string
	Category string
	Region   string
	Unread   bool
	Page     int
	PerPage  int
}

// Offset converts the 1-based page number to a SQL offset.
func (f ItemFilter) Offset() int {
	if f.Page < 2 {
		return 0
	}
	return (f.Page - 1) * f.PerPage
}

// Page bundles a result page with the metadata the list template needs.
type Page struct {
	Items      []Exam
	Total      int
	Page       int
	PerPage    int
	TotalPages int
	HasPrev    bool
	HasNext    bool
}

// Stats is the summary shown at the top of the list page.
type Stats struct {
	TotalExams  int        `json:"total_exams"`
	UnreadExams int        `json:"unread_exams"`
	SourceCount int        `json:"source_count"`
	LastCrawlAt *time.Time `json:"last_crawl_at,omitempty"`
	// FailingSources counts sources whose most recent run failed or was empty.
	FailingSources int `json:"failing_sources"`
}
