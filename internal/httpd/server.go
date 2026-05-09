// Package httpd exposes the minimal HTTP surface the bot needs:
//
//   - GET /health   — cheap liveness probe for Render + UptimeRobot
//   - GET /status   — JSON metrics snapshot
//   - GET /pair     — token-gated QR page for one-time device pairing
//   - GET /         — human-readable landing page
package httpd

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"

	"github.com/skip2/go-qrcode"

	"wbot/internal/config"
	"wbot/internal/sniper"
)

type Server struct {
	cfg *config.Config
	s   *sniper.Sniper
	mux *http.ServeMux
}

func New(cfg *config.Config, s *sniper.Sniper) *Server {
	srv := &Server{cfg: cfg, s: s, mux: http.NewServeMux()}
	srv.mux.HandleFunc("/health", srv.health)
	srv.mux.HandleFunc("/status", srv.status)
	srv.mux.HandleFunc("/pair", srv.pair)
	srv.mux.HandleFunc("/", srv.index)
	return srv
}

func (srv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	srv.mux.ServeHTTP(w, r)
}

// ---------- /health ----------
// UptimeRobot pings this every 5 minutes. Returns 200 only when the WA socket
// is up and authenticated — so an alert fires if the bot has degraded.

func (srv *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if srv.s.IsConnected() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}
	// During pairing, report 200 too so the service isn't recycled by Render
	if srv.s.InPairingMode() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PAIRING"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("DISCONNECTED"))
}

// ---------- /status ----------

func (srv *Server) status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(srv.s.Stats())
}

// ---------- / ----------

func (srv *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w,
		"SNIPER ONLINE\nuptime=%s\nconnected=%v\npairing=%v\ndetected=%d\nreacted=%d\n",
		srv.s.Uptime(), srv.s.IsConnected(), srv.s.InPairingMode(),
		srv.s.Stats()["detected_total"], srv.s.ReactionCount(),
	)
}

// ---------- /pair ----------

var pairTpl = template.Must(template.New("pair").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta http-equiv="refresh" content="25">
  <title>Sniper Pairing</title>
  <style>
    :root { color-scheme: dark; }
    * { box-sizing: border-box; }
    body {
      font-family: system-ui, -apple-system, "Segoe UI", sans-serif;
      background: radial-gradient(ellipse at top, #0f1722 0%, #070a10 100%);
      color: #eaeef2;
      display: flex; align-items: center; justify-content: center;
      min-height: 100vh; margin: 0; padding: 20px;
    }
    .card {
      background: #131a22; padding: 32px; border-radius: 16px;
      text-align: center; max-width: 440px; width: 100%;
      box-shadow: 0 20px 50px rgba(0,0,0,.5);
      border: 1px solid #1f2937;
    }
    h1 { margin: 0 0 8px; font-size: 22px; font-weight: 600; }
    .subtitle { color: #7d8ba0; font-size: 13px; margin-bottom: 24px; }
    img.qr {
      width: 320px; height: 320px; background: #fff;
      border-radius: 12px; padding: 12px; display: block; margin: 0 auto;
    }
    .msg { margin-top: 20px; color: #9aa7b2; font-size: 13px; line-height: 1.6; }
    .ok { color: #4ade80; font-weight: 600; font-size: 18px; }
    .jid { color: #60a5fa; font-family: ui-monospace, monospace; font-size: 12px; word-break: break-all; }
    .wait { color: #fbbf24; font-weight: 500; }
    .steps { text-align: left; background: #0b1119; padding: 16px 20px;
             border-radius: 8px; margin-top: 20px; font-size: 13px;
             color: #c7d2dd; line-height: 1.7; }
    .steps li { margin: 4px 0; }
  </style>
</head>
<body>
  <div class="card">
    <h1>🎯 Sniper Bot — Device Pairing</h1>
    <p class="subtitle">Auto-refresh every 25s while a QR is active.</p>
    {{if .HasQR}}
      <img class="qr" src="data:image/png;base64,{{.QR}}" alt="WhatsApp pairing QR">
      <ol class="steps">
        <li>Open <b>WhatsApp</b> on your phone</li>
        <li>Go to <b>Settings → Linked Devices</b></li>
        <li>Tap <b>Link a device</b></li>
        <li>Point your camera at the QR above</li>
      </ol>
    {{else if .Paired}}
      <p class="ok">✅ Device already linked</p>
      <p class="jid">{{.JID}}</p>
      <p class="msg">This endpoint remains available but no QR is needed until the device is unlinked.</p>
    {{else}}
      <p class="wait">⏳ Waiting for QR code…</p>
      <p class="msg">The bot is starting up. The QR will appear here within a few seconds — this page will auto-refresh.</p>
    {{end}}
  </div>
</body>
</html>`))

func (srv *Server) pair(w http.ResponseWriter, r *http.Request) {
	// Constant-time token check to resist timing attacks
	token := r.URL.Query().Get("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(srv.cfg.PairToken)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	data := struct {
		HasQR  bool
		QR     string
		Paired bool
		JID    string
	}{}

	if srv.s.InPairingMode() {
		code := srv.s.CurrentPairCode()
		if code != "" {
			png, err := qrcode.Encode(code, qrcode.Medium, 320)
			if err != nil {
				http.Error(w, "qr encode failed", http.StatusInternalServerError)
				return
			}
			data.HasQR = true
			data.QR = base64.StdEncoding.EncodeToString(png)
		}
	} else if jid := srv.s.DeviceJID(); jid != "" {
		data.Paired = true
		data.JID = jid
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = pairTpl.Execute(w, data)
}
