// Package identity binds several logins to one account.
//
// Every login provider mints its own actor id — Discord a bare snowflake,
// Google "google:<sub>", GitHub "github:<numeric id>" — so the same human
// logging in three ways used to be three strangers: separate sessions, grants,
// ownership, spend, caps and audit rows.
//
// The fix is deliberately NOT alias-aware comparison. Actor ids are compared
// three different ways across this codebase (config.SameActor for grants, plain
// == for the concurrency cap and the case board's "mine" filter, and as map
// keys for webUsers / inbox files / spend rollups), so teaching one of them
// about aliases would fix a third of the problem while making a pure function
// read mutable locked state. Instead this store is consulted at the moment an
// actor id is BORN — the login callback, the Discord message handler — and
// resolves it to its canonical form there. Every comparison downstream keeps
// working untouched because it never sees an alias at all.
//
// Canonical means "the account you were logged in as when you linked". Existing
// deployments are Discord-first, so a Discord snowflake stays canonical and all
// of that person's history stays theirs: there is no migration.
//
// This does not weaken the per-provider namespace invariant (see
// config.NormalizeActorID): namespaces stay distinct and a link is an explicit,
// audited assertion by the person who owns both logins, never an inference from
// a matching email or handle.
package identity

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoshift/grokwork/internal/atomicfile"
	"github.com/acoshift/grokwork/internal/config"
)

// FileName is the store's file inside the data dir.
const FileName = "identity-links.json"

// MaxAliasesPerCanonical caps how many logins may point at one account. The
// cap is not about a plausible number of logins (nobody has eight); it bounds
// what a stolen session can append to the file before somebody notices.
const MaxAliasesPerCanonical = 8

// Link is one alias's binding to a canonical account.
//
// Handle caches the GitHub login for a "github:" alias (empty for every other
// kind). It is metadata, never identity: a GitHub login can be renamed and the
// freed name re-registered by a stranger, so the numeric id in the alias key is
// the identity and the handle is refreshed from the provider on every login and
// link. Attribution (slice 4) needs it to build the noreply address, which is
// why it is cached here rather than re-fetched.
//
// Every field is a value type, so handing out a Link is already a detached
// copy. A future slice/map/pointer field would break that — see
// sessionstore.Entry.clone for how that lesson was learned there.
type Link struct {
	// Canonical is stored NORMALIZED (config.NormalizeActorID), so a
	// hand-written "42424" and a "discord:42424" cannot become two accounts.
	// Readers get it back in wire form — see Store.Canonical.
	Canonical string    `json:"canonical"`
	Handle    string    `json:"handle,omitempty"`
	LinkedAt  time.Time `json:"linkedAt,omitzero"`
}

// Store is data/identity-links.json: normalized alias actor id → Link.
//
// The map is keyed by the ALIAS because that is the lookup every mint point
// makes ("who is this login?"), and it is what makes one-hop resolution a
// single map read.
type Store struct {
	mu       sync.RWMutex
	filePath string
	links    map[string]Link
	warnings []string
	now      func() time.Time
}

// New loads or creates data/identity-links.json under dataDir.
//
// A malformed file is a hard error rather than a silent reset. Load VALIDATION
// drops individual bad links (see sanitize) because an unresolvable link just
// leaves that person on their unlinked identity, which under-grants — the safe
// direction. Unparseable JSON is different in kind: starting empty would look
// identical and then the next Link call would overwrite the file, destroying
// every binding in it. Refusing to boot is one deleted file away from recovery.
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		filePath: filepath.Join(dataDir, FileName),
		links:    map[string]Link{},
		now:      time.Now,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	// Failing closed is invisible unless it is announced: somebody whose link
	// was dropped simply stops being recognized, which looks like a bug in
	// whatever they were doing.
	for _, w := range s.warnings {
		fmt.Fprintln(os.Stderr, w)
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var stored map[string]Link
	if err := json.Unmarshal(raw, &stored); err != nil {
		return fmt.Errorf("parse %s: %w", FileName, err)
	}
	s.links, s.warnings = sanitize(stored)
	return nil
}

// sanitize validates a freshly decoded file, returning the links to keep (with
// both sides normalized) and a warning per link dropped.
//
// Every rule fails CLOSED — a link it cannot prove unambiguous is dropped, and
// the person falls back to their unlinked identity. Iteration is over sorted
// keys throughout so both the drops and the warnings are deterministic.
func sanitize(stored map[string]Link) (map[string]Link, []string) {
	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, "[warn] identity: "+fmt.Sprintf(format, args...))
	}

	// Pass 1: per-link validity, grouped by normalized alias so two spellings
	// of one alias are visible as a collision.
	type candidate struct {
		rawKey string
		link   Link
	}
	byAlias := map[string][]candidate{}
	for _, rawKey := range slices.Sorted(maps.Keys(stored)) {
		link := stored[rawKey]
		alias := config.NormalizeActorID(rawKey)
		canonical := config.NormalizeActorID(link.Canonical)
		switch {
		case alias == "":
			warn("dropped link keyed %q: not a usable actor id.", rawKey)
		case canonical == "":
			warn("dropped link %q: its canonical id %q is empty or unusable.", rawKey, link.Canonical)
		case alias == canonical:
			warn("dropped self-link %q → %q: an account cannot be its own alias.", rawKey, link.Canonical)
		default:
			link.Canonical = canonical
			byAlias[alias] = append(byAlias[alias], candidate{rawKey: rawKey, link: link})
		}
	}

	// Pass 2: one alias may not be written twice. config.json's team keys take
	// the same line on case-insensitive collisions: two spellings of one key is
	// an operator editing the one they cannot see, and picking a winner here
	// would silently grant one of the two accounts.
	kept := make(map[string]Link, len(byAlias))
	for _, alias := range slices.Sorted(maps.Keys(byAlias)) {
		cands := byAlias[alias]
		if len(cands) > 1 {
			spellings := make([]string, 0, len(cands))
			for _, c := range cands {
				spellings = append(spellings, fmt.Sprintf("%q → %q", c.rawKey, c.link.Canonical))
			}
			warn("dropped %d conflicting links for alias %q (%s): one login cannot belong to two accounts.",
				len(cands), alias, strings.Join(spellings, ", "))
			continue
		}
		kept[alias] = cands[0].link
	}

	// Pass 3: no chains. Resolution is exactly one hop (Canonical does a single
	// map read), so a link whose canonical is itself an alias would resolve to
	// a stale middle account. Dropping is safe in one pass: a drop only ever
	// removes aliases, which can never create a new chain.
	for _, alias := range slices.Sorted(maps.Keys(kept)) {
		link := kept[alias]
		if mid, chained := kept[link.Canonical]; chained {
			warn("dropped chained link %q → %q: %q is itself an alias of %q, and links resolve one hop only.",
				alias, link.Canonical, link.Canonical, mid.Canonical)
			delete(kept, alias)
		}
	}

	// Pass 4: the per-canonical cap, applied in sorted-alias order so the same
	// aliases survive on every boot.
	byCanonical := map[string][]string{}
	for _, alias := range slices.Sorted(maps.Keys(kept)) {
		c := kept[alias].Canonical
		byCanonical[c] = append(byCanonical[c], alias)
	}
	for _, canonical := range slices.Sorted(maps.Keys(byCanonical)) {
		aliases := byCanonical[canonical]
		if len(aliases) <= MaxAliasesPerCanonical {
			continue
		}
		dropped := aliases[MaxAliasesPerCanonical:]
		warn("account %q has %d linked logins, over the cap of %d: dropped %s.",
			canonical, len(aliases), MaxAliasesPerCanonical, strings.Join(quoteAll(dropped), ", "))
		for _, alias := range dropped {
			delete(kept, alias)
		}
	}

	return kept, warnings
}

func quoteAll(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, fmt.Sprintf("%q", id))
	}
	return out
}

// Warnings returns the load-time complaints, already written to stderr. The web
// UI can surface them; a test can assert failing closed actually said so.
func (s *Store) Warnings() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.warnings)
}

// Canonical resolves an actor id to the account it belongs to, or returns it
// unchanged when it is not an alias.
//
// This is called on every Discord message and every web request that mints an
// actor, so it is one read-locked map lookup, and a store with no links at all
// does not even normalize.
//
// The result is in WIRE form — the exact spelling the surfaces use, so a
// canonical Discord account comes back as a bare snowflake and can be compared
// with == against sessions, audit rows and webUsers keys. An id that is not an
// alias is returned VERBATIM: this function has to be a no-op for every caller
// that has one, which it would not be if it normalized on the way out.
func (s *Store) Canonical(actorID string) string {
	if s == nil || actorID == "" {
		return actorID
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.links) == 0 {
		return actorID
	}
	link, ok := s.links[config.NormalizeActorID(actorID)]
	if !ok {
		return actorID
	}
	return wireForm(link.Canonical)
}

// AliasesOf returns the logins linked to an account, in wire form, ordered by
// normalized id so the account page and GitHubFor both see a stable list.
func (s *Store) AliasesOf(canonical string) []string {
	if s == nil {
		return nil
	}
	c := config.NormalizeActorID(canonical)
	if c == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	aliases := s.aliasesOfLocked(c)
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		out = append(out, wireForm(alias))
	}
	return out
}

// aliasesOfLocked returns the normalized aliases of a normalized canonical id.
func (s *Store) aliasesOfLocked(canonical string) []string {
	var out []string
	for alias, link := range s.links {
		if link.Canonical == canonical {
			out = append(out, alias)
		}
	}
	slices.Sort(out)
	return out
}

// GitHubFor returns the GitHub login and numeric id linked to an account.
//
// The numeric id is the alias's own subject (the identity); the login is the
// cached handle. ok is false when the account has no GitHub login linked — or
// when the one it has carries no handle, since "<id>+@users.noreply.github.com"
// is a malformed address and no attribution is better than wrong attribution.
// A second GitHub alias that does carry a handle is still used.
func (s *Store) GitHubFor(canonical string) (login, numericID string, ok bool) {
	if s == nil {
		return "", "", false
	}
	c := config.NormalizeActorID(canonical)
	if c == "" {
		return "", "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, alias := range s.aliasesOfLocked(c) {
		if config.ActorKind(alias) != config.ActorKindGitHub {
			continue
		}
		id := config.ActorSubject(alias)
		handle := s.links[alias].Handle
		if id == "" || handle == "" {
			continue
		}
		return handle, id, true
	}
	return "", "", false
}

// Link binds alias to canonical, refreshing the cached handle.
//
// Re-linking an alias to the account it already belongs to only refreshes the
// handle (a login does this on every sign-in) and keeps the original LinkedAt,
// which is the fact worth remembering. Re-pointing it at a DIFFERENT account is
// refused: silently moving a login would move every grant that resolves through
// it, so the owner has to unlink first.
func (s *Store) Link(alias, canonical, handle string) error {
	if s == nil {
		return fmt.Errorf("identity store is not configured")
	}
	a := config.NormalizeActorID(alias)
	c := config.NormalizeActorID(canonical)
	if a == "" {
		return fmt.Errorf("alias actor id is required")
	}
	if c == "" {
		return fmt.Errorf("canonical actor id is required")
	}
	if a == c {
		return fmt.Errorf("%s cannot be an alias of itself", wireForm(a))
	}
	handle = strings.TrimPrefix(strings.TrimSpace(handle), "@")
	if config.ActorKind(a) != config.ActorKindGitHub {
		// Only a GitHub alias has a handle to cache. Storing one for another
		// kind would put a display name where GitHubFor expects a login.
		handle = ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	prev, existed := s.links[a]
	if existed && prev.Canonical != c {
		return fmt.Errorf("%s is already linked to %s — unlink it first", wireForm(a), wireForm(prev.Canonical))
	}
	// One hop, enforced from both ends: the target must not itself be an alias,
	// and an id that other logins already resolve to must not become one.
	if mid, ok := s.links[c]; ok {
		return fmt.Errorf("%s is itself a linked login of %s — link to that account instead",
			wireForm(c), wireForm(mid.Canonical))
	}
	if owned := s.aliasesOfLocked(a); len(owned) > 0 {
		return fmt.Errorf("%s already has %d linked login(s) and cannot become an alias itself",
			wireForm(a), len(owned))
	}
	if !existed && len(s.aliasesOfLocked(c))+1 > MaxAliasesPerCanonical {
		return fmt.Errorf("%s already has the maximum of %d linked logins", wireForm(c), MaxAliasesPerCanonical)
	}

	next := Link{Canonical: c, Handle: handle, LinkedAt: s.now().UTC()}
	if existed && !prev.LinkedAt.IsZero() {
		next.LinkedAt = prev.LinkedAt
	}
	s.links[a] = next
	if err := s.save(); err != nil {
		// Keep memory and disk agreeing: a link that works until the next
		// restart is worse than one that never took.
		if existed {
			s.links[a] = prev
		} else {
			delete(s.links, a)
		}
		return err
	}
	return nil
}

// Unlink removes a login binding. Unlinking something that is not linked is
// not an error — the account page can be submitted twice.
func (s *Store) Unlink(alias string) error {
	if s == nil {
		return fmt.Errorf("identity store is not configured")
	}
	a := config.NormalizeActorID(alias)
	if a == "" {
		return fmt.Errorf("alias actor id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.links[a]
	if !ok {
		return nil
	}
	delete(s.links, a)
	if err := s.save(); err != nil {
		s.links[a] = prev
		return err
	}
	return nil
}

func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.links, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// This file decides which account a login is, so it is replaced durably,
	// never written in place — see atomicfile.Write.
	return atomicfile.Write(s.filePath, raw, 0o600)
}

// wireForm spells a normalized actor id the way the surfaces do.
//
// It mirrors web.actorIDFor, and must: Discord ids stay BARE everywhere in this
// system (sessions on disk, audit rows, webUsers keys, allowlists, teams,
// per-user capability maps), so handing back "discord:42424…" from Canonical
// would break every plain == and every map lookup this design exists to leave
// alone.
func wireForm(normalizedID string) string {
	if config.IsDiscordActor(normalizedID) {
		return config.ActorSubject(normalizedID)
	}
	return config.NormalizeActorID(normalizedID)
}
