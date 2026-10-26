package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"unicode"

	"github.com/gorilla/websocket"
)

// Diagnostics are returned only on the authenticated, encrypted test connection.
// Bound and flatten text before logging/display; never echo raw command bodies.
func probeDiagnostic(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	if len(text) > 512 {
		text = strings.ToValidUTF8(text[:512], "") + "..."
	}
	return text
}

func (r *ProbeResult) fail(ctx context.Context, err error) {
	r.Error = probeDiagnostic(err.Error())
	r.Detail = r.Error
	var network net.Error
	var closed *websocket.CloseError
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		r.Detail = "connectivity test canceled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		if r.Stage == "command" {
			r.Detail = "timed out waiting for connectivity test command result"
		} else {
			r.Detail = "tunnel connection timed out"
		}
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed), errors.As(err, &closed):
		r.Detail = "test connection closed before a valid result was received"
	}
}

func validProbeReply(reply probeReply, id string) bool {
	if reply.Command != probeResultCommand || reply.TestID != id || reply.Reason > resetUnreachable ||
		(reply.Status != "ok" && reply.Status != "failed") || (reply.Status == "ok") != (reply.Reason == 0) {
		return false
	}
	if reply.Stage != "" && reply.Stage != "backend" && reply.Stage != "command" {
		return false
	}
	if reply.Status == "ok" && (reply.Stage == "command" || reply.Error != "") {
		return false
	}
	return reply.Error == probeDiagnostic(reply.Error) && reply.Target == probeDiagnostic(reply.Target)
}

func (s *Server) rejectProbeCommand(ctx context.Context, ws *websocket.Conn, crypt *codec, request tunnelRequest, err error) {
	s.cfg.Options.Logf("test_id=%s command=%s rejected: stage=command reason=%q", request.id, probeConnectCommand, err)
	if ctx.Err() != nil {
		return
	}
	reply := probeReply{Command: probeResultCommand, TestID: request.id, Status: "failed",
		Stage: "command", Reason: resetDialFailed, Error: probeDiagnostic(err.Error())}
	if err := writeProbeMessage(ws, crypt, reply); err != nil {
		s.cfg.Options.Logf("test_id=%s command=%s error response failed: reason=%v", request.id, probeResultCommand, err)
	}
}
