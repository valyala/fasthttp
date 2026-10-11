package websocket

import (
	"errors"
	"strconv"
)

// MessageType is the type of a data message.
type MessageType int

// Data message types.
const (
	MessageText   MessageType = 1
	MessageBinary MessageType = 2
)

// StatusCode is a close status code (RFC 6455 section 7.4).
type StatusCode int

// Close status codes. StatusNoStatusReceived, StatusAbnormalClosure and
// StatusTLSHandshake never appear in a Close frame and Close rejects them;
// the first two are reported for a Close frame without a status and for a
// connection that ended without a Close frame.
const (
	StatusNormalClosure           StatusCode = 1000
	StatusGoingAway               StatusCode = 1001
	StatusProtocolError           StatusCode = 1002
	StatusUnsupportedData         StatusCode = 1003
	StatusNoStatusReceived        StatusCode = 1005
	StatusAbnormalClosure         StatusCode = 1006
	StatusInvalidFramePayloadData StatusCode = 1007
	StatusPolicyViolation         StatusCode = 1008
	StatusMessageTooBig           StatusCode = 1009
	StatusMandatoryExtension      StatusCode = 1010
	StatusInternalError           StatusCode = 1011
	StatusServiceRestart          StatusCode = 1012
	StatusTryAgainLater           StatusCode = 1013
	StatusBadGateway              StatusCode = 1014
	StatusTLSHandshake            StatusCode = 1015
)

// CloseError is the error of a connection closed with a status code.
type CloseError struct {
	Reason string
	Code   StatusCode
}

// Error returns the status code and reason.
func (e CloseError) Error() string {
	return "websocket: close " + strconv.Itoa(int(e.Code)) + ": " + e.Reason
}

// CloseStatus returns the status code in err, or -1 if err is not a CloseError.
func CloseStatus(err error) StatusCode {
	if ce, ok := errors.AsType[CloseError](err); ok {
		return ce.Code
	}
	return -1
}

// validReceivedCode reports whether code may appear in a Close frame.
func validReceivedCode(code StatusCode) bool {
	switch {
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1014:
		return true
	default:
		return code >= 3000 && code <= 4999
	}
}
