package bot

import (
	"fmt"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/sessionstore"
	"github.com/bwmarrin/discordgo"
)

func TestSnowflakeFromTime(t *testing.T) {
	// 1s after the Discord epoch → 1000 << 22.
	got := snowflakeFromTime(time.UnixMilli(discordEpochMs + 1000))
	if got != "4194304000" {
		t.Fatalf("snowflakeFromTime = %s, want 4194304000", got)
	}
	// Pre-epoch clamps to zero instead of underflowing.
	if got := snowflakeFromTime(time.UnixMilli(0)); got != "0" {
		t.Fatalf("pre-epoch snowflake = %s, want 0", got)
	}
	// Round-trip through discordgo's parser stays within 1ms.
	now := time.Now().UTC().Truncate(time.Millisecond)
	ts, err := discordgo.SnowflakeTimestamp(snowflakeFromTime(now))
	if err != nil {
		t.Fatal(err)
	}
	if !ts.Equal(now) {
		t.Fatalf("round-trip = %s, want %s", ts, now)
	}
}

func TestMarkMessageHandledDedupAndBound(t *testing.T) {
	b := &Bot{}
	if !b.markMessageHandled("m1") {
		t.Fatal("first mark should be new")
	}
	if b.markMessageHandled("m1") {
		t.Fatal("second mark should be duplicate")
	}
	if !b.messageHandled("m1") {
		t.Fatal("peek should see m1")
	}
	// Overflow the cap; the oldest entry is evicted and markable again.
	for i := range handledMsgCap + 1 {
		b.markMessageHandled(fmt.Sprintf("bulk-%d", i))
	}
	if b.messageHandled("m1") {
		t.Fatal("m1 should have been evicted")
	}
	if len(b.handledQ) != handledMsgCap || len(b.handledSet) != handledMsgCap {
		t.Fatalf("bound broken: q=%d set=%d want %d", len(b.handledQ), len(b.handledSet), handledMsgCap)
	}
}

func TestAdvanceCoverageMonotonic(t *testing.T) {
	b := &Bot{}
	t1 := time.UnixMilli(2000)
	t0 := time.UnixMilli(1000)
	b.advanceCoverage(t1)
	b.advanceCoverage(t0) // older — must not regress
	if got := b.coverageMs.Load(); got != 2000 {
		t.Fatalf("coverageMs = %d, want 2000", got)
	}
}

func catchupTestSession(t *testing.T) *discordgo.Session {
	t.Helper()
	s, err := discordgo.New("Bot fake-token")
	if err != nil {
		t.Fatal(err)
	}
	s.State.User = &discordgo.User{ID: "botid", Bot: true}
	if err := s.State.GuildAdd(&discordgo.Guild{ID: "g1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.State.ChannelAdd(&discordgo.Channel{
		ID: "ch1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText,
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCatchupSweepFirstReadyOnlySetsWatermark(t *testing.T) {
	b, _ := testBotWithData(t)
	s := catchupTestSession(t)
	fetched := false
	b.catchupFetch = func(channelID, afterID string) ([]*discordgo.Message, error) {
		fetched = true
		return nil, nil
	}
	b.catchupSweep(s, "ready")
	if fetched {
		t.Fatal("first READY must not fetch history")
	}
	if b.coverageMs.Load() == 0 {
		t.Fatal("first READY should initialize the watermark")
	}
}

func TestCatchupSweepRecoversMissedMention(t *testing.T) {
	b, _ := testBotWithData(t) // maps channel ch1 → project app
	s := catchupTestSession(t)
	b.advanceCoverage(time.Now().UTC().Add(-5 * time.Minute))

	mention := []*discordgo.User{{ID: "botid"}}
	b.catchupFetch = func(channelID, afterID string) ([]*discordgo.Message, error) {
		if channelID != "ch1" {
			return nil, fmt.Errorf("unexpected channel %s", channelID)
		}
		return []*discordgo.Message{
			// Newest-first, as the REST API may return them.
			{ID: "300", ChannelID: "ch1", Author: &discordgo.User{ID: "u1"}, Content: "<@botid> missed task", Mentions: mention},
			{ID: "200", ChannelID: "ch1", Author: &discordgo.User{ID: "botid", Bot: true}, Content: "bot reply"},
			{ID: "100", ChannelID: "ch1", Author: &discordgo.User{ID: "u2"}, Content: "plain chatter"},
		}, nil
	}
	var got []*discordgo.MessageCreate
	b.catchupDispatch = func(_ *discordgo.Session, m *discordgo.MessageCreate) {
		got = append(got, m)
	}

	before := time.Now().UTC().Truncate(time.Millisecond) // watermark stores unix ms
	b.catchupSweep(s, "resumed")

	if len(got) != 1 {
		t.Fatalf("dispatched %d messages, want 1", len(got))
	}
	if got[0].ID != "300" {
		t.Fatalf("dispatched id=%s, want 300", got[0].ID)
	}
	if got[0].GuildID != "g1" {
		t.Fatalf("GuildID = %q, want g1 (REST messages omit it)", got[0].GuildID)
	}
	if wm := time.UnixMilli(b.coverageMs.Load()); wm.Before(before) {
		t.Fatalf("watermark %s not advanced past sweep start %s", wm, before)
	}
}

func TestCatchupSweepSkipsHandledMessages(t *testing.T) {
	b, _ := testBotWithData(t)
	s := catchupTestSession(t)
	b.advanceCoverage(time.Now().UTC().Add(-5 * time.Minute))
	b.markMessageHandled("300")

	b.catchupFetch = func(channelID, afterID string) ([]*discordgo.Message, error) {
		return []*discordgo.Message{
			{ID: "300", ChannelID: "ch1", Author: &discordgo.User{ID: "u1"}, Content: "<@botid> already seen", Mentions: []*discordgo.User{{ID: "botid"}}},
		}, nil
	}
	dispatched := 0
	b.catchupDispatch = func(_ *discordgo.Session, _ *discordgo.MessageCreate) { dispatched++ }

	b.catchupSweep(s, "resumed")
	if dispatched != 0 {
		t.Fatalf("dispatched %d, want 0 (already handled)", dispatched)
	}
}

func TestCatchupSweepRerunsWhenTriggeredMidSweep(t *testing.T) {
	b, _ := testBotWithData(t)
	s := catchupTestSession(t)
	b.advanceCoverage(time.Now().UTC().Add(-5 * time.Minute))

	rounds := 0
	b.catchupFetch = func(channelID, afterID string) ([]*discordgo.Message, error) {
		rounds++
		return nil, nil
	}
	// Simulate a READY/RESUMED that arrived while a sweep was in flight.
	b.catchupRerun.Store(true)
	b.catchupSweep(s, "resumed")
	if rounds != 2 {
		t.Fatalf("fetch rounds = %d, want 2 (initial + rerun)", rounds)
	}
	if b.catchupRerun.Load() {
		t.Fatal("rerun flag should be consumed")
	}
}

func TestCatchupChannelPaginates(t *testing.T) {
	b, _ := testBotWithData(t)
	s := catchupTestSession(t)

	calls := 0
	b.catchupFetch = func(channelID, afterID string) ([]*discordgo.Message, error) {
		calls++
		switch calls {
		case 1:
			if afterID != "0" {
				t.Fatalf("page 1 after=%s, want 0", afterID)
			}
			out := make([]*discordgo.Message, 0, catchupPageSize)
			for i := 1; i <= catchupPageSize; i++ {
				out = append(out, &discordgo.Message{
					ID: fmt.Sprintf("%d", i), ChannelID: "ch1",
					Author: &discordgo.User{ID: "u1"}, Content: "chatter",
				})
			}
			return out, nil
		case 2:
			if afterID != fmt.Sprintf("%d", catchupPageSize) {
				t.Fatalf("page 2 after=%s, want %d", afterID, catchupPageSize)
			}
			return []*discordgo.Message{{
				ID: "999", ChannelID: "ch1",
				Author: &discordgo.User{ID: "u1"}, Content: "<@botid> tail task",
				Mentions: []*discordgo.User{{ID: "botid"}},
			}}, nil
		default:
			t.Fatalf("unexpected page %d", calls)
			return nil, nil
		}
	}
	var got []*discordgo.MessageCreate
	b.catchupDispatch = func(_ *discordgo.Session, m *discordgo.MessageCreate) {
		got = append(got, m)
	}

	scanned, recovered := b.catchupChannel(s, "ch1", "0")
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2", calls)
	}
	if scanned != catchupPageSize+1 || recovered != 1 {
		t.Fatalf("scanned=%d recovered=%d, want %d/1", scanned, recovered, catchupPageSize+1)
	}
	if len(got) != 1 || got[0].ID != "999" {
		t.Fatalf("dispatched %v, want [999]", got)
	}
}

func TestCatchupTargetsChannelsAndRecentThreads(t *testing.T) {
	b, _ := testBotWithData(t) // channel map: ch1 → app
	// UpdatedAt is turn-stamped only; give a recent turn so the thread is in-window.
	if err := b.sessions.Set("thread-1", sessionstore.Entry{
		Project:   "app",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	targets := b.catchupTargets()
	want := map[string]bool{"ch1": false, "thread-1": false}
	for _, id := range targets {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, found := range want {
		if !found {
			t.Fatalf("target %s missing from %v", id, targets)
		}
	}
}
