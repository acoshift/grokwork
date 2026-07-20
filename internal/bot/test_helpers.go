package bot

import (
	"fmt"
	"sync"
	"time"
)

// FakeThreadAPI is a test double for createWorkflowThread.
type FakeThreadAPI struct {
	mu        sync.Mutex
	Sends     []string
	Starts    []string
	FailSend  error
	FailStart error
	NextMsg   string
	NextTh    string
}

func (f *FakeThreadAPI) SendMessage(channelID, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Sends = append(f.Sends, channelID+"|"+content)
	if f.FailSend != nil {
		return "", f.FailSend
	}
	if f.NextMsg == "" {
		f.NextMsg = "msg-1"
	}
	return f.NextMsg, nil
}

func (f *FakeThreadAPI) StartThread(channelID, messageID, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Starts = append(f.Starts, channelID+"|"+messageID+"|"+name)
	if f.FailStart != nil {
		return "", f.FailStart
	}
	if f.NextTh == "" {
		f.NextTh = "thread-1"
	}
	// Unique thread when NextTh is fixed but multiple creates: append counter
	th := f.NextTh
	if len(f.Starts) > 1 {
		th = fmt.Sprintf("%s-%d", f.NextTh, len(f.Starts))
	}
	return th, nil
}

// StartCount returns how many StartThread calls were made.
func (f *FakeThreadAPI) StartCount() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Starts)
}

// SetThreadAPIForTest injects a thread API (or clears with nil). For tests only.
func SetThreadAPIForTest(b *Bot, api threadAPI) {
	if b == nil {
		return
	}
	b.threadAPI = api
}

// FillQueueForTest holds an active job and fills the follow-up queue to capacity.
func FillQueueForTest(b *Bot, threadID, project string) error {
	if b == nil {
		return fmt.Errorf("nil bot")
	}
	job := &runJob{cancel: func() {}, start: time.Now(), project: project}
	claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID})
	if err != nil || !claimed {
		return fmt.Errorf("claim: claimed=%v err=%v", claimed, err)
	}
	for i := 0; i < maxFollowupQueue; i++ {
		c, _, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{threadID: threadID})
		if err != nil || c {
			return fmt.Errorf("enqueue %d: claimed=%v err=%v", i, c, err)
		}
	}
	return nil
}

// WaitIdleForTest blocks until no active jobs remain or timeout elapses.
func WaitIdleForTest(b *Bot, timeout time.Duration) {
	if b == nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		busy := false
		b.states.Range(func(_, value any) bool {
			st, _ := value.(*threadState)
			if st == nil {
				return true
			}
			st.mu.Lock()
			if st.job != nil || len(st.queue) > 0 {
				busy = true
			}
			st.mu.Unlock()
			return !busy
		})
		if !busy {
			// small settle for history file writes
			time.Sleep(30 * time.Millisecond)
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
}
