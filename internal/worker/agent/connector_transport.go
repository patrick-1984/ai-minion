package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Time limits of one connection attempt. Each is per attempt: the connector
// retries forever, so a limit only keeps one stalled attempt from being the
// last one.
const (
	// controlTimeout bounds the wait for the Hub's control frame after the
	// handshake.
	controlTimeout = 10 * time.Second
	// DefaultDialTimeout bounds the dial: TCP connect, TLS handshake and the
	// WebSocket upgrade. A proxy that accepts the connection and then says
	// nothing would otherwise leave the Worker connecting forever.
	DefaultDialTimeout = 30 * time.Second
	// handshakeTimeout bounds the AD-6 handshake after the dial.
	handshakeTimeout = 30 * time.Second
)

// NewHubDialer returns the production [Dialer]: WSS dial (any certificate is
// accepted, A-15), AD-6 handshake in connect mode against the stored pin, the
// control frame (welcome, revoke, not_enrolled or an error frame) read from the
// secure channel, and for a welcome the yamux client session.
//
// The Hub answers inside the channel before yamux starts, so the Dialer does
// not use transport.ConnectWorker, which starts yamux right after the
// handshake.
func NewHubDialer() Dialer {
	return hubDialer{dialTimeout: DefaultDialTimeout, handshakeTimeout: handshakeTimeout}
}

type hubDialer struct {
	dialTimeout      time.Duration
	handshakeTimeout time.Duration
	// afterReply is a test seam that runs right after the Hub's control frame
	// was read; nil in production.
	afterReply func()
}

func (d hubDialer) Connect(ctx context.Context, p ConnectParams) (*Attempt, error) {
	dialCtx, cancelDial := context.WithTimeout(ctx, d.dialTimeout)
	conn, df, err := transport.DialInfo(dialCtx, p.HubURL)
	cancelDial()
	if p.Logger != nil {
		p.Logger.Debug("hub dial", "url", df.URL, "proxy", df.Proxy, "http_status", df.HTTPStatus, "tls", df.TLSVersion,
			"peer_subject", df.PeerSubject, "peer_issuer", df.PeerIssuer, "peer_sha256", df.PeerSHA256,
			"subprotocol", df.Subprotocol, "error", errString(err))
	}
	if err != nil {
		if ctx.Err() == nil && dialCtx.Err() != nil {
			return nil, fmt.Errorf("dial the hub: no answer within %s", d.dialTimeout)
		}
		return nil, err
	}
	// ctx bounds the handshake and the wait for the control frame, and nothing
	// after: the session lives on without it. The handshake has its own
	// deadline, so that a Hub that accepts the upgrade and then stalls cannot
	// hold the attempt either.
	hsTimer := time.AfterFunc(d.handshakeTimeout, func() { _ = conn.Close() })
	defer hsTimer.Stop()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key:       p.Key,
		Name:      p.Name,
		Ops:       p.Ops,
		Features:  p.Features,
		Facts:     p.Facts,
		Protocols: p.Protocols,
		Mode:      transport.ModeConnect,
		Pin:       p.Pin,
		Ratchet:   p.Ratchet,
		Clock:     p.Clock,
	})
	if p.Logger != nil {
		p.Logger.Debug("hub handshake", "ok", err == nil, "error", errString(err))
	}
	if err != nil {
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	stopCh := context.AfterFunc(ctx, func() { _ = res.Channel.Close() })
	stop()
	// The channel is guarded from the end of the handshake (section 11.3): it
	// is closed at the certificate's end, and both clocks are read again before
	// the control frame is accepted. A reply that came in at or after not_after
	// is not handed on, so nothing it says can be committed.
	exp := res.Expiry
	guard, stopGuard := enrollGuard(&exp, p.Clock, func() { _ = res.Channel.Close() })
	defer stopGuard()
	reply, err := readReply(res.Channel)
	if d.afterReply != nil {
		d.afterReply()
	}
	if !stopCh() {
		_ = res.Channel.Close()
		return nil, ctx.Err()
	}
	if guard.check() {
		_ = res.Channel.Close()
		return nil, ErrHubCertExpired
	}
	if err != nil {
		_ = res.Channel.Close()
		return nil, err
	}
	if p.Logger != nil {
		p.Logger.Debug("hub answered", "reply", reply.Type, "protocol", res.Version.String(), "pin_changed", res.PinChanged,
			"ratchet_changed", res.RatchetChanged, "cert_not_after", time.UnixMilli(res.Expiry.WallMs).UTC().Format(time.RFC3339))
	}
	att := &Attempt{
		Pin:            res.Pin,
		PinChanged:     res.PinChanged,
		PinPassed:      res.PinPassed,
		OpPub:          res.OpPub,
		Ratchet:        res.Ratchet,
		RatchetChanged: res.RatchetChanged,
		Expiry:         &res.Expiry,
		TranscriptHash: res.TranscriptHash[:],
		Version:        res.Version,
		Reply:          reply,
		TLS:            df.TLS,
	}
	if reply.Type != proto.TypeWelcome {
		// A revoke and a not_enrolled end the connection.
		_ = res.Channel.Close()
		return att, nil
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, p.Session)
	if err != nil {
		return nil, err
	}
	att.Session = sess
	return att, nil
}

// readReply reads the control frame the Hub sends right after the handshake.
func readReply(ch *transport.Channel) (Reply, error) {
	_ = ch.SetReadDeadline(time.Now().Add(controlTimeout))
	defer func() { _ = ch.SetReadDeadline(time.Time{}) }()
	body, err := proto.ReadFrame(ch, proto.StreamFrameLimit)
	if err != nil {
		return Reply{}, fmt.Errorf("read the hub's answer: %w", err)
	}
	return decodeReply(body)
}

// decodeReply parses a control frame. A Hub error frame becomes a
// *proto.Error.
func decodeReply(body json.RawMessage) (Reply, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return Reply{}, errors.New("the hub's answer is not valid JSON")
	}
	switch head.Type {
	case proto.TypeWelcome:
		var w HubWelcome
		if err := json.Unmarshal(body, &w); err != nil {
			return Reply{}, errors.New("the hub's welcome is malformed")
		}
		// The display texts come from the peer: one line, no control or
		// format characters, bounded (they reach the console, the log and
		// `minion worker status`). OwnerID is compared, not shown.
		w.Owner, w.Name, w.Kind = proto.SafeText(w.Owner, 128), proto.SafeText(w.Name, 255), proto.SafeText(w.Kind, 32)
		if !validWorkerID(w.WorkerID) {
			return Reply{}, errors.New("the hub's welcome has a malformed worker id")
		}
		return Reply{Type: proto.TypeWelcome, Welcome: w}, nil
	case proto.TypeRevoke:
		var r struct {
			Sig identity.B64 `json:"sig"`
		}
		// A revoke without a readable signature is still a revoke frame: the
		// connector ignores it because an empty signature never verifies.
		_ = json.Unmarshal(body, &r)
		return Reply{Type: proto.TypeRevoke, RevokeSig: r.Sig}, nil
	case proto.TypeNotEnrolled:
		return Reply{Type: proto.TypeNotEnrolled}, nil
	case proto.TypeError:
		var f proto.ErrorFrame
		if err := json.Unmarshal(body, &f); err != nil {
			return Reply{}, errors.New("the hub's error answer is malformed")
		}
		return Reply{}, f.Err()
	}
	return Reply{}, fmt.Errorf("the hub sent an unexpected frame of type %q", head.Type)
}

// validWorkerID reports whether id has the shape of a Worker id: 1 to 64
// characters of letters, digits, '.', '_' and '-' (the Hub issues UUIDs). An id
// from the peer that fails this is refused, so it can never carry control
// characters into a box, a log line or `minion worker status`.
func validWorkerID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
