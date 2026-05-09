// Package sniper implements the WhatsApp keyword-monitor bot.
//
// Architecture:
//
//   - All Signal Protocol state is persisted in Postgres via whatsmeow's
//     sqlstore (one SQL transaction per ratchet step → zero torn writes,
//     zero Bad MAC from partial state).
//   - The hot path (event → detect → dispatch reaction) is lock-free
//     using sync/atomic counters.
//   - Reactions are fired in a dedicated goroutine so the event loop
//     never blocks waiting on network I/O.
//   - Self-sent messages are ID-tracked to prevent echo loops when the
//     bot replies to owner commands.
package sniper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"wbot/internal/config"
)

// Sniper is the long-lived bot instance.
type Sniper struct {
	cfg *config.Config
	ctx context.Context

	db        *sql.DB
	container *sqlstore.Container
	client    *whatsmeow.Client

	// Target scope (resolved at init; may be empty if not yet configured)
	targetGroupJID types.JID
	hasTarget      bool // a specific group JID was parsed successfully
	scanAll        bool // global-scan mode: every chat is in scope

	// Pre-computed keyword list (strings.Contains is already highly optimized)
	keywords []string

	// Atomic hot-path metrics (zero allocation, zero locks)
	startedAt      time.Time
	reactionCount  atomic.Uint64
	detectedCount  atomic.Uint64
	lastDetectNs   atomic.Int64
	lastLatencyNs  atomic.Int64
	lastFailureStr atomic.Pointer[string]

	// Pairing state (HTTP /pair reads CurrentPairCode)
	pairMode atomic.Bool
	pairMu   sync.RWMutex
	pairCode string

	// Own-message ID tracking (prevents echo loops on owner command replies)
	ownMsgIDs sync.Map // map[string]time.Time
}

// New builds the Sniper but does NOT connect yet. Call Start() to connect.
func New(ctx context.Context, cfg *config.Config) (*Sniper, error) {
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	// Start background heartbeat to prevent Neon compute suspension
	go keepDatabaseAlive(ctx, db)

	// whatsmeow logger — pipe to slog for unified structured logs
	waLogger := newSlogWALog(slog.Default().With("src", "wa"))

	container := sqlstore.NewWithDB(db, "postgres", waLogger)
	if err := container.Upgrade(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlstore migrate: %w", err)
	}

	s := &Sniper{
		cfg:       cfg,
		ctx:       ctx,
		db:        db,
		container: container,
		keywords:  cfg.TargetKeywords,
		startedAt: time.Now(),
	}

	switch {
	case cfg.ScanAll:
		s.scanAll = true
	case cfg.TargetGroupJID != "":
		j, err := types.ParseJID(cfg.TargetGroupJID)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("parse TARGET_GROUP_JID: %w", err)
		}
		s.targetGroupJID = j
		s.hasTarget = true
	}

	return s, nil
}

// Start connects to WhatsApp. If no device is registered yet, enters pairing
// mode (QR will be served on the HTTP /pair endpoint).
func (s *Sniper) Start() error {
	device, err := s.container.GetFirstDevice(s.ctx)
	if err != nil {
		return fmt.Errorf("get device: %w", err)
	}

	waLogger := newSlogWALog(slog.Default().With("src", "wa-client"))
	s.client = whatsmeow.NewClient(device, waLogger)
	s.client.EnableAutoReconnect = true
	s.client.AutoTrustIdentity = true
	// Honor PreRetryCallback so we don't spam retries when the server is truly down
	s.client.AddEventHandler(s.handleEvent)

	// Start the pruner goroutine for our own-message ID tracking
	go s.pruneOwnMsgIDs()

	if s.client.Store.ID == nil {
		// Unpaired — enter QR flow
		s.pairMode.Store(true)
		qrChan, err := s.client.GetQRChannel(s.ctx)
		if err != nil {
			return fmt.Errorf("get qr channel: %w", err)
		}
		go s.pairLoop(qrChan)
		if err := s.client.Connect(); err != nil {
			return fmt.Errorf("connect (pairing mode): %w", err)
		}
		slog.Info("awaiting pairing — open /pair?token=… on your phone")
		return nil
	}

	// Already paired — connect and go hot
	if err := s.client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	scope := s.cfg.TargetGroupJID
	if s.scanAll {
		scope = "<all-chats>"
	}
	slog.Info("connected to whatsapp",
		"jid", s.client.Store.ID.String(),
		"target_scope", scope,
		"keywords", s.keywords,
	)

	// Prime the group metadata cache so the first inbound hit doesn't
	// pay the participant-resolution round-trip.
	if s.hasTarget {
		go s.warmGroup()
	}

	return nil
}

// Close releases resources cleanly. Safe to call multiple times.
func (s *Sniper) Close() {
	if s.client != nil {
		s.client.Disconnect()
	}
	if s.db != nil {
		_ = s.db.Close()
	}
}

// warmGroup resolves group metadata proactively so the first real hit
// doesn't incur a participant-resolution round-trip on the hot path.
func (s *Sniper) warmGroup() {
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()

	info, err := s.client.GetGroupInfo(ctx, s.targetGroupJID)
	if err != nil {
		slog.Warn("warm group metadata failed (non-fatal)",
			"jid", s.targetGroupJID.String(), "err", err)
		return
	}
	slog.Info("group metadata warmed",
		"name", info.Name,
		"participants", len(info.Participants),
	)
}

// ---------- Public accessors (for HTTP server) ----------

func (s *Sniper) IsConnected() bool {
	return s.client != nil && s.client.IsConnected() && s.client.IsLoggedIn()
}

func (s *Sniper) InPairingMode() bool {
	return s.pairMode.Load()
}

func (s *Sniper) CurrentPairCode() string {
	s.pairMu.RLock()
	defer s.pairMu.RUnlock()
	return s.pairCode
}

func (s *Sniper) DeviceJID() string {
	if s.client == nil || s.client.Store.ID == nil {
		return ""
	}
	return s.client.Store.ID.String()
}

func (s *Sniper) Uptime() time.Duration {
	return time.Since(s.startedAt).Round(time.Second)
}

func (s *Sniper) ReactionCount() uint64 {
	return s.reactionCount.Load()
}

// Stats snapshots runtime metrics for /status.
func (s *Sniper) Stats() map[string]any {
	var lastErr string
	if p := s.lastFailureStr.Load(); p != nil {
		lastErr = *p
	}
	return map[string]any{
		"connected":       s.IsConnected(),
		"logged_in":       s.client != nil && s.client.IsLoggedIn(),
		"pairing_mode":    s.InPairingMode(),
		"device_jid":      s.DeviceJID(),
		"target_group":    s.cfg.TargetGroupJID,
		"scan_all":        s.scanAll,
		"keywords":        s.keywords,
		"uptime_seconds":  int64(s.Uptime().Seconds()),
		"detected_total":  s.detectedCount.Load(),
		"reactions_total": s.reactionCount.Load(),
		"last_detect_ns":  s.lastDetectNs.Load(),
		"last_latency_ms": float64(s.lastLatencyNs.Load()) / 1e6,
		"last_failure":    lastErr,
	}
}

// ---------- Error helpers ----------

func (s *Sniper) setLastFailure(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	s.lastFailureStr.Store(&msg)
}

// ensureConnected is a belt-and-suspenders guard used by outbound paths.
func (s *Sniper) ensureConnected() error {
	if s.client == nil {
		return errors.New("client not initialized")
	}
	if !s.client.IsConnected() {
		return errors.New("client not connected")
	}
	if !s.client.IsLoggedIn() {
		return errors.New("client not logged in")
	}
	return nil
}

// ---------- whatsmeow logger → slog adapter ----------

type slogWALog struct{ l *slog.Logger }

func newSlogWALog(l *slog.Logger) waLog.Logger { return &slogWALog{l: l} }

func (w *slogWALog) Errorf(msg string, args ...any) { w.l.Error(fmt.Sprintf(msg, args...)) }
func (w *slogWALog) Warnf(msg string, args ...any)  { w.l.Warn(fmt.Sprintf(msg, args...)) }
func (w *slogWALog) Infof(msg string, args ...any)  { w.l.Info(fmt.Sprintf(msg, args...)) }
func (w *slogWALog) Debugf(msg string, args ...any) { w.l.Debug(fmt.Sprintf(msg, args...)) }
func (w *slogWALog) Sub(mod string) waLog.Logger   { return &slogWALog{l: w.l.With("mod", mod)} }

// keepDatabaseAlive periodically queries the DB to prevent Neon compute suspension.
func keepDatabaseAlive(ctx context.Context, db *sql.DB) {
	ticker := time.NewTicker(3 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("database heartbeat stopped")
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			var dummy int
			err := db.QueryRowContext(pingCtx, "SELECT 1").Scan(&dummy)
			cancel()

			if err != nil {
				slog.Warn("database heartbeat failed (non-fatal)", "err", err)
			}
		}
	}
}
