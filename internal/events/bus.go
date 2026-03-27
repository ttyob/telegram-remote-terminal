package events

import (
	"sync"
	"time"
)

type OutputEvent struct {
	ChatID    int64     `json:"chat_id"`
	Data      string    `json:"data"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
}

type Bus struct {
	mu          sync.RWMutex
	defaultSize int
	subs        map[int64]map[chan OutputEvent]struct{}
	history     map[int64][]OutputEvent
	historyTTL  time.Duration
}

func NewBus(defaultSize int, historyTTL time.Duration) *Bus {
	if defaultSize <= 0 {
		defaultSize = 100
	}
	if historyTTL <= 0 {
		historyTTL = 24 * time.Hour
	}
	return &Bus{
		defaultSize: defaultSize,
		subs:        make(map[int64]map[chan OutputEvent]struct{}),
		history:     make(map[int64][]OutputEvent),
		historyTTL:  historyTTL,
	}
}

func (b *Bus) Subscribe(chatID int64) (<-chan OutputEvent, func()) {
	ch := make(chan OutputEvent, b.defaultSize)

	b.mu.Lock()
	if _, ok := b.subs[chatID]; !ok {
		b.subs[chatID] = make(map[chan OutputEvent]struct{})
	}
	b.subs[chatID][ch] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		chatSubs, ok := b.subs[chatID]
		if !ok {
			return
		}
		if _, ok := chatSubs[ch]; !ok {
			return
		}
		delete(chatSubs, ch)
		close(ch)
		if len(chatSubs) == 0 {
			delete(b.subs, chatID)
		}
	}

	return ch, unsubscribe
}

func (b *Bus) Publish(evt OutputEvent) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}

	b.mu.Lock()
	h := append(b.history[evt.ChatID], evt)
	h = b.pruneHistoryLocked(h, evt.Timestamp)
	b.history[evt.ChatID] = h

	chatSubs := b.subs[evt.ChatID]
	if len(chatSubs) == 0 {
		b.mu.Unlock()
		return
	}
	subs := make([]chan OutputEvent, 0, len(chatSubs))
	for ch := range chatSubs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (b *Bus) Snapshot(chatID int64) []OutputEvent {
	b.mu.Lock()
	b.history[chatID] = b.pruneHistoryLocked(b.history[chatID], time.Now())
	h := b.history[chatID]
	if len(h) == 0 {
		b.mu.Unlock()
		return nil
	}
	out := make([]OutputEvent, len(h))
	copy(out, h)
	b.mu.Unlock()
	return out
}

func (b *Bus) pruneHistoryLocked(h []OutputEvent, now time.Time) []OutputEvent {
	if len(h) == 0 || b.historyTTL <= 0 {
		return h
	}

	cutoff := now.Add(-b.historyTTL)
	idx := 0
	for idx < len(h) && h[idx].Timestamp.Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return h
	}
	if idx >= len(h) {
		return nil
	}
	out := make([]OutputEvent, len(h)-idx)
	copy(out, h[idx:])
	return out
}
