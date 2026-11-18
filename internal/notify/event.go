package notify

import "time"

// Kind 是对外事件类型，取值与 lingo.EventKind 一致。
type Kind string

const (
	TicketCreated  Kind = "ticket.created"
	TicketStatus   Kind = "ticket.status"
	TicketComment  Kind = "ticket.comment"
	TicketSchedule Kind = "ticket.scheduled"
	ReviewDecision Kind = "review.decision"
	TicketReleased Kind = "ticket.released"
	TicketDeployed Kind = "ticket.deployed"
	TriageRequired Kind = "triage.required"
	TriageOverdue  Kind = "triage.overdue"
	TriageResolved Kind = "triage.resolved"
)

// Event 是写入 events 表、经 outbox 投递给接入方的事件；字段与 lingo.Event 一一对应，由根包转换。
// ID 与 At 由存储层在写入时分配。
type Event struct {
	ID         int64
	Kind       Kind
	At         time.Time
	ProjectID  int64
	ProjectKey string
	TicketID   int64
	TicketType string
	Title      string
	Status     string
	PrevStatus string
	Action     string
	UserID     string
	ActorID    string
	Body       string
	URL        string
}
