package web

import (
	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/inbox"
)

// inboxPage lists the viewer's queued notifications.
//
// This is where a notification lands when no push channel can reach the
// recipient — which is every member who signed in without Discord, since a DM
// needs a snowflake. Without somewhere to read them, "queued instead of dropped"
// would still mean never told.
func (s *Server) inboxPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Inbox"
	d.IsInbox = true

	actor := s.fixActor(ctx)
	if actor.ID != "" && s.bot != nil {
		if store := s.bot.Inbox(); store != nil {
			items, err := store.List(actor.ID)
			if err != nil {
				// A malformed feed must not take the page down; show it empty.
				d.Error = "could not read your inbox"
			} else {
				d.InboxItems = items
			}
		}
	}
	return s.viewPage(ctx, "inbox", d)
}

// InboxItems is the view type for the inbox list.
type InboxItems = []inbox.Item
