package watcher

import "time"

// FileChangeMsg is a Bubble Tea message for file changes.
type FileChangeMsg struct {
	Path      string
	Operation Operation
	Time      time.Time
}

// NewFileChangeMsg creates a new file change message.
func NewFileChangeMsg(path string, op Operation) FileChangeMsg {
	return FileChangeMsg{
		Path:      path,
		Operation: op,
		Time:      time.Now(),
	}
}

// WatcherStats holds watcher statistics.
type WatcherStats struct {
	Running      bool
	WatchedPaths int
	EventsCount  int64
}

// EventBuffer is a circular buffer for recent events.
type EventBuffer struct {
	events []Event
	size   int
	head   int
	count  int
}

// NewEventBuffer creates a new event buffer with the given capacity.
func NewEventBuffer(capacity int) *EventBuffer {
	if capacity < 1 {
		capacity = 100
	}
	return &EventBuffer{
		events: make([]Event, capacity),
		size:   capacity,
	}
}

// Add adds an event to the buffer.
func (b *EventBuffer) Add(event Event) {
	b.events[b.head] = event
	b.head = (b.head + 1) % b.size
	if b.count < b.size {
		b.count++
	}
}

// Recent returns the n most recent events.
func (b *EventBuffer) Recent(n int) []Event {
	if n > b.count {
		n = b.count
	}
	if n <= 0 {
		return nil
	}

	result := make([]Event, n)
	for i := 0; i < n; i++ {
		idx := (b.head - n + i + b.size) % b.size
		result[i] = b.events[idx]
	}

	return result
}

// Clear clears the event buffer.
func (b *EventBuffer) Clear() {
	b.head = 0
	b.count = 0
}

// Len returns the number of events in the buffer.
func (b *EventBuffer) Len() int {
	return b.count
}
