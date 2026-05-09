package sniper

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// handleEvent is the single entry point registered with whatsmeow.
// Dispatching is done via type switch for zero-reflection overhead.
func (s *Sniper) handleEvent(raw any) {
	switch e := raw.(type) {
	case *events.Message:
		s.handleMessage(e)
	case *events.Connected:
		slog.Info("[wa] connected", "jid", s.DeviceJID())
	case *events.Disconnected:
		slog.Warn("[wa] disconnected — auto-reconnect will engage")
	case *events.LoggedOut:
		slog.Error("[wa] logged out — device needs to be re-paired", "reason", e.Reason)
		s.pairMode.Store(true)
	case *events.StreamReplaced:
		slog.Error("[wa] stream replaced — another session took over")
	case *events.TemporaryBan:
		slog.Error("[wa] temporary ban",
			"code", e.Code, "expires", e.Expire)
	case *events.ClientOutdated:
		slog.Warn("[wa] client outdated — update whatsmeow dependency")
	case *events.PairSuccess:
		slog.Info("[wa] pair success", "jid", e.ID.String())
		s.pairMode.Store(false)
		s.pairMu.Lock()
		s.pairCode = ""
		s.pairMu.Unlock()
		// Warm the group once the pairing lands
		if s.hasTarget {
			go s.warmGroup()
		}
	}
}

// handleMessage is the hot path. Keep it allocation-light and branch-predictable.
func (s *Sniper) handleMessage(e *events.Message) {
	t0 := time.Now()

	// (1) Own-message echo guard — skip anything the bot itself sent.
	if e.Info.IsFromMe && s.isOwnMsgID(e.Info.ID) {
		return
	}

	// (2) Owner command branch — runs for ANY message IsFromMe that is NOT
	//     one of our own outbound replies. This lets the user issue commands
	//     like "!id", "!status", "!ping" from their own phone.
	if e.Info.IsFromMe {
		if s.handleOwnerCommand(e) {
			return
		}
		// Own message but not a command → ignore (prevents self-triggering on target group sends)
		return
	}

	// (3) Scope filter — three modes:
	//       a) scanAll  → accept every chat (groups + DMs)
	//       b) hasTarget → accept ONLY the configured group
	//       c) neither  → no scope configured, drop everything
	switch {
	case s.scanAll:
		// fall through to keyword matching
	case !s.hasTarget:
		return
	case e.Info.Chat.User != s.targetGroupJID.User ||
		e.Info.Chat.Server != s.targetGroupJID.Server:
		return
	}

	// (4) Extract text (no allocation unless necessary — getters return zero on nil)
	text := messageText(e)
	if text == "" {
		return
	}

	fmt.Printf("DEBUG: Incoming message from %s: %s\n", e.Info.Sender.String(), text)
	fmt.Printf("DEBUG: Checking message for keywords...\n")

	// (5) AND-match all keywords. strings.Contains is SIMD-optimized in the Go runtime.
	for i := range s.keywords {
		if !strings.Contains(text, s.keywords[i]) {
			return
		}
	}

	// (6) HIT — record detection latency, dispatch reaction asynchronously.
	detectNs := time.Since(t0).Nanoseconds()
	s.lastDetectNs.Store(detectNs)
	s.detectedCount.Add(1)

	slog.Info("🎯 TARGET DETECTED",
		"detect_us", float64(detectNs)/1000,
		"chat", e.Info.Chat.String(),
		"sender", e.Info.Sender.String(),
		"msg_id", e.Info.ID,
	)

	// Fire-and-forget so the event loop never blocks on network I/O.
	// Uses the parent context so shutdown propagates.
	go s.fireReaction(e.Info.Chat, e.Info.Sender, e.Info.ID)
}

// fireReaction sends the ✅ reaction. Any error here is a real send failure
// (not a silent Ghost Reaction) because whatsmeow returns error on server NACK.
func (s *Sniper) fireReaction(chat, sender types.JID, msgID types.MessageID) {
	t0 := time.Now()

	if err := s.ensureConnected(); err != nil {
		s.setLastFailure(err)
		slog.Error("reaction skipped — not connected", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	reaction := s.client.BuildReaction(chat, sender, msgID, "✅")
	resp, err := s.client.SendMessage(ctx, chat, reaction)
	if err != nil {
		s.setLastFailure(err)
		slog.Error("reaction send failed",
			"err", err, "chat", chat.String(), "msg_id", msgID)
		return
	}

	// Track our own outbound message ID so we don't loop on the echo
	s.trackOwnMsgID(resp.ID)

	lat := time.Since(t0).Nanoseconds()
	s.lastLatencyNs.Store(lat)
	s.reactionCount.Add(1)

	slog.Info("✅ REACTION CONFIRMED",
		"send_ms", float64(lat)/1e6,
		"server_timestamp", resp.Timestamp,
		"msg_id", msgID,
		"out_id", resp.ID,
	)
}

// messageText extracts plain text from conversation or extended text messages.
// Uses generated protobuf getters that safely handle nil pointers.
func messageText(e *events.Message) string {
	msg := e.Message
	if msg == nil {
		return ""
	}
	if t := msg.GetConversation(); t != "" {
		return t
	}
	return msg.GetExtendedTextMessage().GetText()
}
