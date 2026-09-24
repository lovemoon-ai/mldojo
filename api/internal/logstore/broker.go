package logstore

import "sync"

// Broker is a tiny topic-based pub/sub. Subscribers receive values on a
// buffered channel; slow subscribers drop messages (log subscribers re-read
// from files, so a dropped notification only delays them).
type Broker struct {
	mu   sync.Mutex
	subs map[string]map[chan any]struct{}
}

func NewBroker() *Broker { return &Broker{subs: map[string]map[chan any]struct{}{}} }

func LogTopic(runID string) string    { return "logs:" + runID }
func MetricTopic(runID string) string { return "metrics:" + runID }
func StatusTopic(runID string) string { return "status:" + runID }
func GPUTopic(nodeID string) string   { return "gpu:" + nodeID }
func AgentTopic(nodeID string) string { return "agent:" + nodeID }

func (b *Broker) Subscribe(topic string) (chan any, func()) {
	ch := make(chan any, 64)
	b.mu.Lock()
	if b.subs[topic] == nil {
		b.subs[topic] = map[chan any]struct{}{}
	}
	b.subs[topic][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[topic], ch)
		if len(b.subs[topic]) == 0 {
			delete(b.subs, topic)
		}
		b.mu.Unlock()
	}
}

func (b *Broker) Publish(topic string, v any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[topic] {
		select {
		case ch <- v:
		default:
		}
	}
}
