package store

import (
	"sync"
	"time"
)

const subscriberBuffer = 32

// Broker fans out change notifications to subscribers. Notifications for the
// same topic are coalesced: the first one schedules a publish after the
// debounce delay, and further ones are absorbed until it fires.
type Broker struct {
	delay time.Duration

	mu      sync.Mutex
	subs    map[chan string]struct{}
	pending map[string]bool
	closed  bool
}

// NewBroker returns a Broker that coalesces notifications within delay.
func NewBroker(delay time.Duration) *Broker {
	return &Broker{
		delay:   delay,
		subs:    make(map[chan string]struct{}),
		pending: make(map[string]bool),
	}
}

// Notify schedules a publish of each topic.
func (b *Broker) Notify(topics ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for _, topic := range topics {
		if b.pending[topic] {
			continue
		}
		b.pending[topic] = true
		time.AfterFunc(b.delay, func() { b.publish(topic) })
	}
}

func (b *Broker) publish(topic string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, topic)
	if b.closed {
		return
	}
	for ch := range b.subs {
		select {
		case ch <- topic:
		default: // slow subscriber; it catches up on the next event or reconnect
		}
	}
}

// Subscribe returns a channel receiving published topics and a function to
// unsubscribe. The channel is closed when the broker is closed.
func (b *Broker) Subscribe() (<-chan string, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan string, subscriberBuffer)
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// Close closes all subscriber channels and drops further notifications.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}
