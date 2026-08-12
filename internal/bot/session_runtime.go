package bot

import (
	"slices"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// SessionRuntimeInfo is the live run state for one unit (API status).
type SessionRuntimeInfo struct {
	Running  bool
	QueueLen int
	Activity string
}

// SessionRuntime loads one threadState (never LoadOrStore) and does not scan
// StatusSnapshot or leak LiveText.
func (b *Bot) SessionRuntime(threadID string) SessionRuntimeInfo {
	if b == nil || threadID == "" {
		return SessionRuntimeInfo{}
	}
	v, ok := b.states.Load(threadID)
	if !ok {
		return SessionRuntimeInfo{}
	}
	st := v.(*threadState)
	st.mu.Lock()
	defer st.mu.Unlock()
	info := SessionRuntimeInfo{QueueLen: len(st.queue), Running: st.job != nil}
	if st.job != nil {
		st.job.mu.Lock()
		info.Activity = st.job.activity
		st.job.mu.Unlock()
	}
	return info
}

func tokenCollaborators(ent *sessionstore.Entry) []string {
	if ent == nil {
		return nil
	}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || config.ActorKind(id) != config.ActorKindToken {
			return
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	add(ent.OwnerID)
	for _, id := range ent.CoOwnerIDs {
		add(id)
	}
	add(ent.CreatedBy)
	return ids
}
