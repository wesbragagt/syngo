package runner

import (
	"sync/atomic"

	"github.com/wesbragagt/syngo/internal/app/ports"
)

// byteCounter is an EventSink that sums the size of every completed transfer,
// which is the real number of payload bytes a node received over the
// network, exactly what the puller recorded chunk by chunk.
type byteCounter struct {
	bytes atomic.Int64
}

func newByteCounter() *byteCounter { return &byteCounter{} }

// Publish implements ports.EventSink.
func (b *byteCounter) Publish(e ports.Event) {
	if e.Kind != "transfer.completed" {
		return
	}
	switch size := e.Fields["size"].(type) {
	case int64:
		b.bytes.Add(size)
	case int:
		b.bytes.Add(int64(size))
	}
}

// Bytes returns the running total.
func (b *byteCounter) Bytes() int64 { return b.bytes.Load() }

var _ ports.EventSink = (*byteCounter)(nil)
