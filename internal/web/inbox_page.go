package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/inbox"
)

// InboxRow is one feed item plus unread standing for this viewer.
type InboxRow struct {
	inbox.Item
	Unread bool
}

// InboxItems is the view type for the inbox list.
type InboxItems = []InboxRow

// inboxPage lists the viewer's notifications. GET does not mark them read.
func (s *Server) inboxPage(ctx *hime.Context) error {
	d := s.inboxPageData(ctx)
	return s.viewPage(ctx, "inbox", d)
}

func (s *Server) partialInboxList(ctx *hime.Context) error {
	return s.viewFragment(ctx, "inbox", "inbox_list", s.inboxPageData(ctx))
}

func (s *Server) inboxPageData(ctx *hime.Context) pageData {
	d := s.basePage(ctx)
	d.Title = "Inbox"
	d.IsInbox = true
	d.InboxItems = s.inboxRows(ctx, &d)
	return d
}

func (s *Server) inboxRows(ctx *hime.Context, d *pageData) []InboxRow {
	actor := s.fixActor(ctx)
	if actor.ID == "" || s.bot == nil {
		return nil
	}
	store := s.bot.Inbox()
	if store == nil {
		return nil
	}
	items, err := store.List(actor.ID)
	if err != nil {
		if d != nil {
			d.Error = "could not read your inbox"
		}
		return nil
	}
	cur := store.ReadCursor(actor.ID)
	var out []InboxRow
	for _, it := range items {
		if !s.inboxItemVisible(ctx, it) {
			continue
		}
		out = append(out, InboxRow{Item: it, Unread: cur.Unread(it.Seq)})
	}
	return out
}

func (s *Server) inboxItemVisible(ctx *hime.Context, it inbox.Item) bool {
	if strings.TrimSpace(it.Project) == "" {
		return true
	}
	return s.ensureProjectAccess(ctx, it.Project) == nil
}

func (s *Server) inboxUnreadVisible(ctx *hime.Context) int {
	n := 0
	for _, row := range s.inboxRows(ctx, nil) {
		if row.Unread {
			n++
		}
	}
	return n
}

// postInboxRead marks one seq or the whole feed read. GET must not write.
func (s *Server) postInboxRead(ctx *hime.Context) error {
	if !s.cfg.WebAuthEnabled() {
		return ctx.Redirect("/inbox")
	}
	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	if sess == nil || !s.checkCSRF(ctx.Request, sess) || !sameOriginRequest(ctx.Request) {
		return ctx.Status(http.StatusForbidden).Error("forbidden")
	}
	actor := s.fixActor(ctx)
	if actor.ID == "" || s.bot == nil || s.bot.Inbox() == nil {
		return ctx.Redirect("/inbox")
	}
	store := s.bot.Inbox()
	if strings.TrimSpace(ctx.PostFormValue("all")) != "" {
		_ = store.MarkAllRead(actor.ID)
	} else if raw := strings.TrimSpace(ctx.PostFormValue("seq")); raw != "" {
		seq, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || seq <= 0 {
			return ctx.Status(http.StatusBadRequest).Error("invalid seq")
		}
		_ = store.MarkRead(actor.ID, seq)
	} else {
		return ctx.Status(http.StatusBadRequest).Error("seq or all required")
	}
	next := safeLocalNext(ctx.PostFormValue("next"))
	if next == "" || next == "/" {
		next = "/inbox"
	}
	return ctx.Redirect(next)
}
