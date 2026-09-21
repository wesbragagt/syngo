package ports

import "time"

// Event is a structured notification emitted by the application layer.
type Event struct {
	Kind   string
	At     time.Time
	Fields map[string]any
}

// EventSink receives events. Publish must not block.
type EventSink interface {
	Publish(e Event)
}
