package utils

import (
	"log/slog"
	"sync"
)

// Dispatcher serializes callback invocations onto a single goroutine so
// callers can safely re-enter the API from inside a callback without
// deadlocking against locks held by the goroutine that produced the event.
//
// The queue is unbounded: Submit never blocks and never drops. A dropped
// OnStreamEnd would leave a chat silent forever, so a slow callback only
// delays later events, it never loses them.
type Dispatcher struct {
	log    *slog.Logger
	wake   chan struct{} // 1-slot: the queue became non-empty, or Close ran
	done   chan struct{}
	queue  []func()
	mu     sync.Mutex
	once   sync.Once
	closed bool
}

// NewDispatcher starts the dispatcher goroutine. bufSize is the initial
// queue capacity.
func NewDispatcher(bufSize int, log *slog.Logger) *Dispatcher {
	if bufSize <= 0 {
		bufSize = 256
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := &Dispatcher{
		log:   log,
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
		queue: make([]func(), 0, bufSize),
	}
	go d.loop()
	return d
}

func (d *Dispatcher) loop() {
	defer close(d.done)
	var batch []func()
	for range d.wake {
		for {
			// Swap the queue with the drained batch so steady state
			// reuses two backing arrays instead of allocating.
			d.mu.Lock()
			batch, d.queue = d.queue, batch[:0]
			closed := d.closed
			d.mu.Unlock()
			if len(batch) == 0 {
				if closed {
					return
				}
				break
			}
			for i, fn := range batch {
				d.run(fn)
				batch[i] = nil
			}
			if cap(batch) > 4096 {
				batch = nil // don't pin a burst-sized array forever
			}
		}
	}
}

func (d *Dispatcher) run(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("dispatcher: callback panic", slog.Any("recover", r))
		}
	}()
	fn()
}

// Submit enqueues fn for execution on the dispatcher goroutine. Never
// blocks and never drops; events run in submission order. A no-op after
// Close.
func (d *Dispatcher) Submit(fn func()) {
	if fn == nil {
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.queue = append(d.queue, fn)
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Close stops the dispatcher and waits for the loop goroutine to drain
// any pending events. Must not be called from inside a callback.
func (d *Dispatcher) Close() {
	d.once.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()
		select {
		case d.wake <- struct{}{}:
		default:
		}
	})
	<-d.done
}
