package http2

import "time"

// streamWorker runs one stream handler at a time.
//
// Every stream needs its own goroutine so a slow handler cannot stall frame
// processing, but creating one per request allocates. Workers are parked
// between streams and reused, following the same shape as the root package's
// workerPool.
//
// The free list is only ever touched by the connection owner: startHandler
// takes from it, and a worker is returned when the owner processes the
// handler's completion command, so neither needs a lock.
type streamWorker struct {
	conn        *serverConn
	stream      chan *serverStream
	lastUseTime time.Time
}

// maxIdleStreamWorkerDuration bounds how long a parked worker survives without
// a stream, so a burst of concurrency doesn't pin goroutines for the life of
// the connection.
const maxIdleStreamWorkerDuration = 10 * time.Second

// startHandler runs the stream's handler on a pooled worker.
func (c *serverConn) startHandler(stream *serverStream) {
	stream.handlerStarted = true
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
	// One slot of buffer: a worker parked in run takes the stream without the
	// owner waiting, but a worker created just above has not reached that
	// receive yet, and the owner must not block on it being scheduled.
	worker := &streamWorker{conn: c, stream: make(chan *serverStream, 1)}
	c.allWorkers = append(c.allWorkers, worker)
	c.workers.Go(worker.run)
	return worker
}

// releaseStreamWorker parks a worker for reuse. It runs on the connection
// owner, after the handler's completion command has been processed. The free
// list is a stack, so the workers at its tail are the ones that go idle.
func (c *serverConn) releaseStreamWorker(stream *serverStream) {
	worker := stream.worker
	if worker == nil {
		return
	}
	stream.worker = nil
	worker.lastUseTime = time.Now()
	c.idleWorkers = append(c.idleWorkers, worker)
}

// reapIdleStreamWorkers stops workers parked for longer than
// maxIdleStreamWorkerDuration. The free list is ordered by release time, so
// the expired workers are a prefix of it.
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

// stopStreamWorkers releases every parked and running worker. A worker inside a
// handler exits once that handler returns, so this must precede workers.Wait.
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
