package http2

import "math"

// connFlowState is the flow-control and peer-settings accounting shared by
// the server and client connections. Methods are pure state transitions: the
// owner provides its own synchronisation and writes any resulting frames.
type connFlowState struct {
	peerInitialStreamWindow  int64
	peerConnectionWindow     int64
	peerMaxFrameSize         int
	peerMaxHeaderListSize    uint64
	peerMaxConcurrentStreams uint32
	receiveConnectionWindow  int64
	pendingConnectionUpdate  int64
	receivedSettings         bool
}

// newConnFlowState seeds the RFC 9113 defaults that apply to a peer before
// its SETTINGS arrive, and this side's advertised connection window.
func newConnFlowState(receiveWindow int64) connFlowState {
	return connFlowState{
		peerInitialStreamWindow:  65535,
		peerConnectionWindow:     65535,
		peerMaxFrameSize:         defaultMaxFrameSize,
		peerMaxHeaderListSize:    math.MaxUint32,
		peerMaxConcurrentStreams: math.MaxUint32,
		receiveConnectionWindow:  receiveWindow,
	}
}

// debitReceiveWindow charges an incoming DATA frame against the connection
// receive window and reports whether the peer stayed within it.
func (f *connFlowState) debitReceiveWindow(flowLength int64) bool {
	f.receiveConnectionWindow -= flowLength
	return f.receiveConnectionWindow >= 0
}

func (f *connFlowState) restoreReceiveWindow(amount int64) {
	f.receiveConnectionWindow += amount
}

// consumeReceived accumulates consumed bytes and, once half the configured
// window is pending, refills the receive window and returns the increment the
// owner must announce with a connection-level WINDOW_UPDATE.
func (f *connFlowState) consumeReceived(amount, windowSize int64) int64 {
	f.pendingConnectionUpdate += amount
	if f.pendingConnectionUpdate < windowSize/2 {
		return 0
	}
	increment := f.pendingConnectionUpdate
	f.pendingConnectionUpdate = 0
	f.receiveConnectionWindow += increment
	return increment
}

// creditSendWindow applies a peer WINDOW_UPDATE and reports whether the send
// window stayed within RFC 9113's 2^31-1 bound.
func (f *connFlowState) creditSendWindow(increment uint32) bool {
	if f.peerConnectionWindow+int64(increment) > math.MaxInt32 {
		return false
	}
	f.peerConnectionWindow += int64(increment)
	return true
}

// streamFlowState is the per-stream window accounting shared by server and
// client streams, under the owner's synchronisation like connFlowState.
type streamFlowState struct {
	sendWindow          int64
	recvWindow          int64
	pendingWindowUpdate int64
}

func (s *streamFlowState) debitReceiveWindow(flowLength int64) bool {
	s.recvWindow -= flowLength
	return s.recvWindow >= 0
}

func (s *streamFlowState) consumeReceived(amount, windowSize int64) int64 {
	s.pendingWindowUpdate += amount
	if s.pendingWindowUpdate < windowSize/2 {
		return 0
	}
	increment := s.pendingWindowUpdate
	s.pendingWindowUpdate = 0
	s.recvWindow += increment
	return increment
}

func (s *streamFlowState) creditSendWindow(increment uint32) bool {
	if s.sendWindow+int64(increment) > math.MaxInt32 {
		return false
	}
	s.sendWindow += int64(increment)
	return true
}

// nextDataChunk sizes the next DATA frame against the peer's frame limit and
// both send windows, debiting them for the returned amount. Zero with pending
// data means flow control blocks the stream right now.
func nextDataChunk(conn *connFlowState, stream *streamFlowState, dataLen int) int {
	if dataLen == 0 || conn.peerConnectionWindow <= 0 || stream.sendWindow <= 0 {
		return 0
	}
	amount := min(dataLen, conn.peerMaxFrameSize, int(conn.peerConnectionWindow), int(stream.sendWindow))
	conn.peerConnectionWindow -= int64(amount)
	stream.sendWindow -= int64(amount)
	return amount
}
