package http2

import "time"

// streamWorker runs one stream handler at a time, parked between streams so a
// slow handler cannot stall frame processing. The free list needs no lock:
// only the connection owner touches it.
type streamWorker struct {
	conn        *serverConn
	stream      chan *serverStream
	lastUseTime time.Time
}

// maxIdleStreamWorkerDuration keeps a burst of concurrency from pinning
// goroutines for the life of the connection.
const maxIdleStreamWorkerDuration = 10 * time.Second

func (c *serverConn) startHandler(stream *serverStream) {
	stream.handlerStarted = true
	stream.handlerGen = c.handlerGen
	c.pendingHandlers++
	worker := c.acquireStreamWorker()
	stream.worker = worker
	worker.stream <- stream
}

func (c *serverConn) acquireStreamWorker() *streamWorker {
	if last := len(c.idleWorkers) - 1; last >= 0 {
		worker := c.idleWorkers[last]
		c.idleWorkers[last] = nil
		c.idleWorkers = c.idleWorkers[:last]
		return worker
	}
	// Buffered: this worker has not reached its receive yet, and the owner
	// must not wait for it to be scheduled.
	worker := &streamWorker{conn: c, stream: make(chan *serverStream, 1)}
	c.allWorkers = append(c.allWorkers, worker)
	c.workers.Go(worker.run)
	return worker
}

// releaseStreamWorker parks a worker for reuse. The free list is a stack, so
// the workers at its tail are the ones that go idle.
func (c *serverConn) releaseStreamWorker(stream *serverStream) {
	worker := stream.worker
	if worker == nil {
		return
	}
	stream.worker = nil
	worker.lastUseTime = c.cycleTime
	c.idleWorkers = append(c.idleWorkers, worker)
}

// reapIdleStreamWorkers stops workers parked too long. The free list is
// ordered by release time, so they are a prefix of it.
func (c *serverConn) reapIdleStreamWorkers() {
	if len(c.idleWorkers) == 0 {
		return
	}
	expiry := time.Now().Add(-maxIdleStreamWorkerDuration)
	expired := 0
	for expired < len(c.idleWorkers) && c.idleWorkers[expired].lastUseTime.Before(expiry) {
		expired++
	}
	if expired == 0 {
		return
	}
	for _, worker := range c.idleWorkers[:expired] {
		close(worker.stream)
		c.forgetStreamWorker(worker)
	}
	remaining := copy(c.idleWorkers, c.idleWorkers[expired:])
	clear(c.idleWorkers[remaining:])
	c.idleWorkers = c.idleWorkers[:remaining]
}

func (c *serverConn) forgetStreamWorker(worker *streamWorker) {
	for i, candidate := range c.allWorkers {
		if candidate != worker {
			continue
		}
		last := len(c.allWorkers) - 1
		c.allWorkers[i] = c.allWorkers[last]
		c.allWorkers[last] = nil
		c.allWorkers = c.allWorkers[:last]
		return
	}
}

// stopStreamWorkers must precede workers.Wait; a worker inside a handler exits
// once that handler returns.
func (c *serverConn) stopStreamWorkers() {
	for _, worker := range c.allWorkers {
		close(worker.stream)
	}
	c.allWorkers = nil
	c.idleWorkers = nil
}

func (w *streamWorker) run() {
	for stream := range w.stream {
		w.conn.runHandler(stream)
	}
}
