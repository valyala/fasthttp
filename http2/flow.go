package http2

import "math"

// recvWindow is one direction's receive accounting: the credit the peer may
// still spend, and the consumed bytes not yet announced back to it.
type recvWindow struct {
	window  int64
	pending int64
}

// debit reports whether the peer stayed inside the window.
func (w *recvWindow) debit(length int64) bool {
	w.window -= length
	return w.window >= 0
}

func (w *recvWindow) restore(amount int64) {
	w.window += amount
}

// consume returns the WINDOW_UPDATE increment the owner must send, or 0.
func (w *recvWindow) consume(amount, windowSize int64) int64 {
	w.pending += amount
	if w.pending < windowSize/2 {
		return 0
	}
	increment := w.pending
	w.pending = 0
	w.window += increment
	return increment
}

// sendWindow is the credit this side may still spend.
type sendWindow struct {
	window int64
}

// credit rejects an increment that overflows RFC 9113's 2^31-1 bound.
func (w *sendWindow) credit(increment uint32) bool {
	if w.window+int64(increment) > math.MaxInt32 {
		return false
	}
	w.window += int64(increment)
	return true
}

// connFlowState is the flow-control and peer-settings accounting shared by the
// server and client connections. The owner synchronises it and writes frames.
type connFlowState struct {
	recv recvWindow
	send sendWindow

	peerInitialStreamWindow  int64
	peerMaxFrameSize         int
	peerMaxHeaderListSize    uint64
	peerMaxConcurrentStreams uint32
	receivedSettings         bool
}

// newConnFlowState seeds the RFC 9113 defaults that apply before the peer's
// SETTINGS arrive.
func newConnFlowState(receiveWindow int64) connFlowState {
	return connFlowState{
		recv:                     recvWindow{window: receiveWindow},
		send:                     sendWindow{window: 65535},
		peerInitialStreamWindow:  65535,
		peerMaxFrameSize:         defaultMaxFrameSize,
		peerMaxHeaderListSize:    math.MaxUint32,
		peerMaxConcurrentStreams: math.MaxUint32,
	}
}

// reserveDataChunk debits both send windows for the amount it returns. Zero
// with pending data means flow control blocks the stream.
func (f *connFlowState) reserveDataChunk(stream *streamFlowState, dataLen int) int {
	blocked := dataLen == 0 || f.send.window <= 0 || stream.send.window <= 0
	if blocked {
		return 0
	}
	amount := min(dataLen, f.peerMaxFrameSize, int(f.send.window), int(stream.send.window))
	f.send.window -= int64(amount)
	stream.send.window -= int64(amount)
	return amount
}

// streamFlowState is the per-stream window accounting, synchronised like
// connFlowState.
type streamFlowState struct {
	recv recvWindow
	send sendWindow
}
