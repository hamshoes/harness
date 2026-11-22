package sessions

import (
	"sync"

	"github.com/hamshoes/harness/internal/store"
)

const subBuffer = 1024

// Sub is one follower's live feed. C is closed when the session is stopped, or when the follower fell
// behind (Lagged is then true and it should re-read the log from where it got to).
type Sub struct {
	C      chan store.Event
	Lagged bool // read only after C is closed
}

// broadcaster fans a session's new events out to its followers.
type broadcaster struct {
	mu     sync.Mutex
	subs   map[*Sub]struct{}
	closed bool
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: map[*Sub]struct{}{}}
}

// subscribe returns a feed of events published from now on.
func (b *broadcaster) subscribe() *Sub {
	s := &Sub{C: make(chan store.Event, subBuffer)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(s.C)
		return s
	}
	b.subs[s] = struct{}{}
	return s
}

func (b *broadcaster) unsubscribe(s *Sub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.C)
	}
}

// publish never blocks: a follower whose buffer is full is dropped as lagged.
func (b *broadcaster) publish(ev store.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.C <- ev:
		default:
			s.Lagged = true
			delete(b.subs, s)
			close(s.C)
		}
	}
}

// close ends every feed; later subscribers get a closed feed at once.
func (b *broadcaster) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for s := range b.subs {
		delete(b.subs, s)
		close(s.C)
	}
}
