package bot

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestGatewayHeartbeatStale(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	maxAge := 90 * time.Second

	if gatewayHeartbeatStale(time.Time{}, now, maxAge) {
		t.Fatal("zero lastAck should not be stale")
	}
	if gatewayHeartbeatStale(now.Add(-30*time.Second), now, maxAge) {
		t.Fatal("fresh ACK should not be stale")
	}
	if !gatewayHeartbeatStale(now.Add(-91*time.Second), now, maxAge) {
		t.Fatal("ACK older than maxAge should be stale")
	}
	if gatewayHeartbeatStale(now.Add(-2*time.Minute), now, 0) {
		t.Fatal("maxAge<=0 should never report stale")
	}
}

func TestDiscordErrLooksLikeDeadConn(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("HTTP 404 Not Found, {\"message\": \"Unknown Channel\"}"), false},
		{errors.New("HTTP 403 Forbidden"), false},
		{errors.New("read tcp 1.2.3.4:1->5.6.7.8:443: connection reset by peer"), true},
		{errors.New("websocket: close 1006 (abnormal closure): unexpected EOF"), true},
		{errors.New("HTTP 503 Service Unavailable, upstream connect error"), true},
		{&net.OpError{Op: "read", Err: errors.New("connection refused")}, true},
		{errors.New("i/o timeout"), true},
	}
	for _, tc := range cases {
		got := discordErrLooksLikeDeadConn(tc.err)
		if got != tc.want {
			t.Errorf("discordErrLooksLikeDeadConn(%v)=%v want %v", tc.err, got, tc.want)
		}
	}
}

func TestStartingStatus(t *testing.T) {
	got := startingStatus("homeconnect")
	if got != "Starting · **homeconnect**…" {
		t.Fatalf("got %q", got)
	}
}
