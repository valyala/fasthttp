package http2

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"net"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
)

var upgradeResponse = []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")

// maxUpgradeSettingsBytes bounds the decoded HTTP2-Settings payload.
const maxUpgradeSettingsBytes = 64 * 6

type serverUpgrade struct {
	request  *fasthttp.Request
	settings []xhttp2.Setting
}

// UpgradeConn serves an RFC 7540 3.2 h2c Upgrade. A malformed HTTP2-Settings
// header declines the upgrade and the request is served as HTTP/1.
func (h *serverHandler) UpgradeConn(
	ctx *fasthttp.ProtocolServerContext,
	c net.Conn,
	upgraded *fasthttp.Request,
) (bool, error) {
	settings, ok := upgradeSettings(upgraded)
	if !ok {
		return false, nil
	}
	conn := newServerConn(ctx, c, &h.config)
	return true, conn.serve(&serverUpgrade{request: upgraded, settings: settings})
}

func upgradeSettings(request *fasthttp.Request) ([]xhttp2.Setting, bool) {
	if !headerValueHasToken(request.Header.Peek(fasthttp.HeaderConnection), "HTTP2-Settings") {
		return nil, false
	}
	values := request.Header.PeekAll("HTTP2-Settings")
	if len(values) != 1 {
		return nil, false
	}
	payload := make([]byte, base64.RawURLEncoding.DecodedLen(len(values[0])))
	n, err := base64.RawURLEncoding.Decode(payload, values[0])
	if err != nil || n%6 != 0 || n > maxUpgradeSettingsBytes {
		return nil, false
	}
	settings := make([]xhttp2.Setting, 0, n/6)
	for i := 0; i < n; i += 6 {
		settings = append(settings, xhttp2.Setting{
			ID:  xhttp2.SettingID(binary.BigEndian.Uint16(payload[i:])),
			Val: binary.BigEndian.Uint32(payload[i+2:]),
		})
	}
	return settings, true
}

func headerValueHasToken(value []byte, token string) bool {
	for len(value) != 0 {
		part := value
		if comma := bytes.IndexByte(value, ','); comma >= 0 {
			part = value[:comma]
			value = value[comma+1:]
		} else {
			value = nil
		}
		if bytes.EqualFold(bytes.TrimSpace(part), []byte(token)) {
			return true
		}
	}
	return false
}

func (c *serverConn) bootstrapUpgradedStream(request *fasthttp.Request) {
	stream := newServerStream(c, 1)
	stream.priority = priority{urgency: 3}
	stream.remoteClosed = true
	stream.maxBody = c.server.MaxRequestBodySize
	if stream.maxBody <= 0 {
		stream.maxBody = fasthttp.DefaultMaxRequestBodySize
	}
	requestCtx := c.protocolContext.AcquireRequestCtx(c.conn, stream)
	stream.request = requestCtx
	request.CopyTo(&requestCtx.Request)
	requestCtx.Request.Header.Del(fasthttp.HeaderConnection)
	requestCtx.Request.Header.Del(fasthttp.HeaderUpgrade)
	requestCtx.Request.Header.Del("HTTP2-Settings")
	c.streams[1] = stream
	c.lastClientStreamID = 1
	c.lastProcessedID = 1
	c.startHandler(stream)
}

var _ fasthttp.ProtocolUpgrader = (*serverHandler)(nil)
