package web

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// eventRing is a per-user circular buffer of recent broadcast events.
// Used to replay missed events when a client reconnects with ?since=<lastEventId>.
const eventRingSize = 256

type bufferedEvent struct {
	ID   uint64
	Data []byte // pre-marshalled JSON (already includes "id" field)
}

type eventBuffer struct {
	mu      sync.Mutex
	rings   map[int64]*ring
	counter atomic.Uint64
}

type ring struct {
	items [eventRingSize]bufferedEvent
	head  int // next write index
	count int // number of valid items (<= eventRingSize)
}

func newEventBuffer() *eventBuffer {
	return &eventBuffer{rings: make(map[int64]*ring)}
}

// nextID returns a globally unique, monotonically increasing event id.
func (b *eventBuffer) nextID() uint64 {
	return b.counter.Add(1)
}

// store appends an event to the user's ring buffer.
func (b *eventBuffer) store(uid int64, id uint64, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.rings[uid]
	if !ok {
		r = &ring{}
		b.rings[uid] = r
	}
	r.items[r.head] = bufferedEvent{ID: id, Data: data}
	r.head = (r.head + 1) % eventRingSize
	if r.count < eventRingSize {
		r.count++
	}
}

// since returns all events for uid with ID > since, in chronological order.
func (b *eventBuffer) since(uid int64, since uint64) [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.rings[uid]
	if !ok || r.count == 0 {
		return nil
	}
	out := make([][]byte, 0, r.count)
	start := (r.head - r.count + eventRingSize) % eventRingSize
	for i := 0; i < r.count; i++ {
		idx := (start + i) % eventRingSize
		ev := r.items[idx]
		if ev.ID > since {
			out = append(out, ev.Data)
		}
	}
	return out
}

// addIDToEvent re-marshals an event map/struct with an injected "id" field.
// Returns the JSON bytes and the assigned id.
func (b *eventBuffer) addIDToEvent(event any) (uint64, []byte, error) {
	id := b.nextID()

	// Try to inject id without re-marshalling for the common map[string]any case.
	if m, ok := event.(map[string]any); ok {
		// Don't mutate caller's map — clone shallowly with id added.
		clone := make(map[string]any, len(m)+1)
		for k, v := range m {
			clone[k] = v
		}
		clone["id"] = id
		data, err := json.Marshal(clone)
		return id, data, err
	}
	if m, ok := event.(map[string]string); ok {
		clone := make(map[string]any, len(m)+1)
		for k, v := range m {
			clone[k] = v
		}
		clone["id"] = id
		data, err := json.Marshal(clone)
		return id, data, err
	}

	// Generic path: marshal then splice "id" in. Fallback only.
	data, err := json.Marshal(event)
	if err != nil {
		return 0, nil, err
	}
	return id, injectIDIntoJSON(data, id), nil
}

// injectIDIntoJSON inserts "id":<id> at the start of a top-level JSON object.
// If the input is not a JSON object, returns it unchanged.
func injectIDIntoJSON(data []byte, id uint64) []byte {
	if len(data) < 2 || data[0] != '{' {
		return data
	}
	idField := []byte(`"id":`)
	idStr := []byte{}
	idStr = appendUint(idStr, id)
	out := make([]byte, 0, len(data)+len(idField)+len(idStr)+1)
	out = append(out, '{')
	out = append(out, idField...)
	out = append(out, idStr...)
	if len(data) > 2 { // not "{}"
		out = append(out, ',')
	}
	out = append(out, data[1:]...)
	return out
}

func appendUint(dst []byte, n uint64) []byte {
	if n == 0 {
		return append(dst, '0')
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return append(dst, buf[i:]...)
}
