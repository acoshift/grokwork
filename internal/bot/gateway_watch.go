package bot

import (
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Gateway health: discordgo already heartbeats on the websocket. That is the
// real "ping". We watch LastHeartbeatAck and force Close+Open when the ACK is
// stale (zombie TCP / dead heartbeat goroutine). REST failures that look like
// network death also trigger a reconnect.

const (
	gatewayWatchInterval = 30 * time.Second
	// Discord heartbeat interval is typically ~41s. Allow a few missed cycles
	// before forcing a reconnect so we do not thrash on brief lag.
	gatewayHeartbeatMaxStale = 90 * time.Second
)

var gatewayWatchOnce sync.Once

// startGatewayWatch starts a single background loop that forces reconnect when
// the gateway heartbeat goes silent. Safe to call from onReady repeatedly.
func (b *Bot) startGatewayWatch() {
	if b == nil {
		return
	}
	gatewayWatchOnce.Do(func() {
		go b.gatewayWatchLoop()
	})
}

func (b *Bot) gatewayWatchLoop() {
	log.Printf("bg: gateway watch interval=%s max_stale=%s", gatewayWatchInterval, gatewayHeartbeatMaxStale)
	ticker := time.NewTicker(gatewayWatchInterval)
	defer ticker.Stop()
	for {
		if b.stopping.Load() {
			return
		}
		b.checkGatewayHealth()
		select {
		case <-ticker.C:
		}
		if b.stopping.Load() {
			return
		}
	}
}

func (b *Bot) checkGatewayHealth() {
	s := b.Discord()
	if s == nil || b.stopping.Load() {
		return
	}
	s.RLock()
	lastAck := s.LastHeartbeatAck
	dataReady := s.DataReady
	s.RUnlock()

	now := time.Now().UTC()
	if gatewayHeartbeatStale(lastAck, now, gatewayHeartbeatMaxStale) {
		age := now.Sub(lastAck)
		log.Printf("gateway: heartbeat ACK stale age=%s dataReady=%v — forcing reconnect", age.Round(time.Second), dataReady)
		b.forceGatewayReconnect(s, "stale heartbeat ACK")
	}
}

// gatewayHeartbeatStale reports whether lastAck is older than maxAge.
// Zero lastAck means "not yet connected" and is not treated as stale.
func gatewayHeartbeatStale(lastAck, now time.Time, maxAge time.Duration) bool {
	if lastAck.IsZero() || maxAge <= 0 {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Sub(lastAck) > maxAge
}

// forceGatewayReconnect closes the gateway websocket and opens a new one.
// discordgo's Close does not auto-reconnect (only read/heartbeat failures do),
// so we must Open again ourselves.
func (b *Bot) forceGatewayReconnect(s *discordgo.Session, reason string) {
	if b == nil || s == nil {
		return
	}
	if b.stopping.Load() {
		return
	}
	if !b.reconnectMu.TryLock() {
		return
	}
	defer b.reconnectMu.Unlock()
	if b.stopping.Load() {
		return
	}

	log.Printf("gateway: force reconnect (%s)", reason)
	if err := s.Close(); err != nil {
		// Close can fail if already closed; still try Open.
		log.Printf("gateway: close: %v", err)
	}

	// Brief pause so Discord accepts a new Identify/Resume cleanly.
	time.Sleep(500 * time.Millisecond)
	if b.stopping.Load() {
		return
	}

	if err := s.Open(); err != nil {
		if errors.Is(err, discordgo.ErrWSAlreadyOpen) {
			log.Printf("gateway: already open after force reconnect")
			return
		}
		log.Printf("gateway: open after force reconnect failed: %v", err)
		return
	}
	log.Printf("gateway: reconnected after force reconnect")
}

// maybeForceReconnectOnDiscordErr triggers a gateway reconnect when a Discord
// REST/API error looks like a dead connection (not 4xx application errors).
func (b *Bot) maybeForceReconnectOnDiscordErr(err error) {
	if b == nil || err == nil || b.stopping.Load() {
		return
	}
	if !discordErrLooksLikeDeadConn(err) {
		return
	}
	s := b.Discord()
	if s == nil {
		return
	}
	log.Printf("gateway: discord API error suggests dead conn: %v", err)
	go b.forceGatewayReconnect(s, "discord API connection error")
}

func discordErrLooksLikeDeadConn(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	// Common transport / proxy failures; avoid matching HTTP 4xx body text.
	for _, needle := range []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"i/o timeout",
		"tls handshake timeout",
		"no such host",
		"unexpected eof",
		"use of closed network connection",
		"websocket: close",
		"http2: client connection lost",
		"server misbehaving",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	// Cloudflare / edge 502/503/504 often mean the path to Discord is broken.
	if strings.Contains(msg, "http 502") ||
		strings.Contains(msg, "http 503") ||
		strings.Contains(msg, "http 504") ||
		strings.Contains(msg, "status code 502") ||
		strings.Contains(msg, "status code 503") ||
		strings.Contains(msg, "status code 504") {
		return true
	}
	return false
}
