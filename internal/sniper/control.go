package sniper

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Owner command prefix. Commands sent from your own phone starting with this
// prefix are consumed by the bot; other messages from yourself are ignored.
const cmdPrefix = "!"

// handleOwnerCommand returns true if the message was a recognized command.
// All commands require e.Info.IsFromMe — the caller guarantees this.
func (s *Sniper) handleOwnerCommand(e *events.Message) bool {
	text := strings.TrimSpace(messageText(e))
	if text == "" || !strings.HasPrefix(text, cmdPrefix) {
		return false
	}

	cmd := strings.ToLower(strings.TrimPrefix(text, cmdPrefix))
	cmd = strings.TrimSpace(cmd)

	switch cmd {
	case "id":
		reply := fmt.Sprintf(
			"🎯 Chat JID: `%s`\n👤 Sender JID: `%s`\n🆔 Msg ID: `%s`",
			e.Info.Chat.String(), e.Info.Sender.String(), e.Info.ID,
		)
		slog.Info("[cmd] !id",
			"chat", e.Info.Chat.String(), "sender", e.Info.Sender.String())
		s.replyTo(e.Info.Chat, reply)
		return true

	case "status":
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		reply := fmt.Sprintf(
			"📊 *Sniper Status*\n"+
				"• Uptime: %s\n"+
				"• Connected: %v\n"+
				"• Detected: %d\n"+
				"• Reacted: %d\n"+
				"• Last detect: %dµs\n"+
				"• Last send: %.2fms\n"+
				"• Heap alloc: %d KB\n"+
				"• Goroutines: %d",
			s.Uptime(),
			s.IsConnected(),
			s.detectedCount.Load(),
			s.reactionCount.Load(),
			s.lastDetectNs.Load()/1000,
			float64(s.lastLatencyNs.Load())/1e6,
			m.Alloc/1024,
			runtime.NumGoroutine(),
		)
		slog.Info("[cmd] !status",
			"detected", s.detectedCount.Load(),
			"reacted", s.reactionCount.Load())
		s.replyTo(e.Info.Chat, reply)
		return true

	case "ping":
		s.replyTo(e.Info.Chat, "pong ⚡")
		return true

	case "help":
		s.replyTo(e.Info.Chat,
			"*Sniper Commands*\n"+
				"`!id` — show current chat + sender JIDs\n"+
				"`!status` — runtime metrics\n"+
				"`!ping` — liveness check\n"+
				"`!help` — this message")
		return true
	}

	// Prefix was present but command unknown — log, swallow, don't echo.
	slog.Warn("[cmd] unknown", "cmd", cmd)
	return true
}

// replyTo sends a text reply to the given chat and tracks the outbound ID
// so the echo guard can skip it on the inbound handler.
func (s *Sniper) replyTo(chat types.JID, text string) {
	if err := s.ensureConnected(); err != nil {
		slog.Error("replyTo skipped — not connected", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	msg := &waProto.Message{
		Conversation: proto.String(text),
	}
	resp, err := s.client.SendMessage(ctx, chat, msg)
	if err != nil {
		s.setLastFailure(err)
		slog.Error("replyTo failed", "err", err, "chat", chat.String())
		return
	}
	s.trackOwnMsgID(resp.ID)
}

// ---------- Own-message ID tracking ----------
// Prevents the bot from reacting to its own outbound messages (which arrive
// back through the event stream with IsFromMe == true).

const ownMsgTTL = 2 * time.Minute

func (s *Sniper) trackOwnMsgID(id types.MessageID) {
	s.ownMsgIDs.Store(string(id), time.Now())
}

func (s *Sniper) isOwnMsgID(id types.MessageID) bool {
	_, ok := s.ownMsgIDs.Load(string(id))
	return ok
}

// pruneOwnMsgIDs walks the map every minute and evicts entries older than ownMsgTTL.
// Keeps memory bounded regardless of reaction volume.
func (s *Sniper) pruneOwnMsgIDs() {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			s.ownMsgIDs.Range(func(k, v any) bool {
				if ts, ok := v.(time.Time); ok && now.Sub(ts) > ownMsgTTL {
					s.ownMsgIDs.Delete(k)
				}
				return true
			})
		}
	}
}
