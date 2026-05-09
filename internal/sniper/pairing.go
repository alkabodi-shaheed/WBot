package sniper

import (
	"log/slog"

	"go.mau.fi/whatsmeow"
)

// pairLoop consumes the QR channel returned by whatsmeow and stores the
// latest QR code string so the HTTP /pair endpoint can render it on demand.
//
// whatsmeow rotates the QR every ~30 seconds ("timeout" event); we silently
// keep replacing the stored code. On "success" we clear and exit.
func (s *Sniper) pairLoop(ch <-chan whatsmeow.QRChannelItem) {
	for evt := range ch {
		switch evt.Event {
		case "code":
			s.pairMu.Lock()
			s.pairCode = evt.Code
			s.pairMu.Unlock()
			slog.Info("[pair] new QR available at /pair endpoint")

		case "success":
			s.pairMu.Lock()
			s.pairCode = ""
			s.pairMu.Unlock()
			s.pairMode.Store(false)
			slog.Info("[pair] device linked successfully")
			return

		case "timeout":
			slog.Warn("[pair] current QR expired — new one will be issued")

		case "err-client-outdated":
			slog.Error("[pair] client outdated — upgrade whatsmeow dependency and redeploy")

		case "err-scanned-without-multidevice":
			slog.Error("[pair] multi-device must be enabled on your WhatsApp account")

		default:
			slog.Info("[pair] event", "event", evt.Event)
		}
	}
}
