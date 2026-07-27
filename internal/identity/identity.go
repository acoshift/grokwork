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
//
// The file also carries a small cache of GitHub logins (see Handle), keyed by
// GitHub actor id rather than by alias. It is the mutable half of git
// attribution, so it is proved by an OAuth round trip, stamped with when, and
// expires — a link is permanent, a name is not.
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

// MaxHandleAge is how long a cached GitHub login may be believed.
//
// The handle is the only mutable half of attribution: GitHub lets an account
// rename and lets the freed name be re-registered by anybody, immediately. It is
// re-proven whenever that person signs in WITH GitHub — but a Discord- or
// Google-first user may never do that again after linking once, and a person who
// works only from Discord may never touch the web at all. Their handle would then
// be believed forever, and the three surfaces that render "@login" (the PR footer,
// the "On behalf of @" comment, `gh pr edit --add-reviewer`) would publicly name
// whoever owns that name now.
//
// So a handle expires. Past this window GitHubFor reports nothing, every caller
// omits the trailer and the mention, and the person gets attribution back by
// signing in with GitHub once. That is the design's own safe default — no
// attribution beats wrong attribution, which looks like it worked. The numeric id
// never expires because it cannot go stale.
const MaxHandleAge = 30 * 24 * time.Hour

// handleRecheckFloor is how stale a still-correct handle must be before a
// re-proof rewrites the file. Without it every sign-in would write, just to move
// a timestamp; with it the common path stays one read lock and no disk I/O.
const handleRecheckFloor = time.Hour

// Link is one alias's binding to a canonical account.
//
// Every field is a value type, so handing out a Link is already a detached
// copy. A future slice/map/pointer field would break that — see
// sessionstore.Entry.clone for how that lesson was learned there.
type Link struct {
	// Canonical is stored NORMALIZED (config.NormalizeActorID), so a
	// hand-written "42424" and a "discord:42424" cannot become two accounts.
	// Readers get it back in wire form — see Store.Canonical.
	Canonical string    `json:"canonical"`
	LinkedAt  time.Time `json:"linkedAt,omitzero"`
}

// Handle caches the GitHub login proved for one "github:<numeric id>" actor id.
//
// It is metadata, never identity: the numeric id in the KEY is the identity, and
// the login is a renameable label on it. Attribution needs the label to build the
// noreply address and to write "@login", which is why it is cached rather than
// re-fetched on every run.
//
// Keyed by the GitHub actor id rather than by an alias, because an account whose
// canonical id IS its GitHub login (a GitHub-first signup) has no alias to hang
// this off, and used to get no attribution at all with no way to fix it.
//
// CheckedAt is when the login was last proved by an OAuth round trip — link or
// sign-in. It is what MaxHandleAge is measured against, so a record with no
// stamp (a file written before this existed) reads as unproven, not as fresh.
type Handle struct {
	Login     string    `json:"login"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// storeFile is the on-disk shape of data/identity-links.json.
//
// The file used to BE the alias→link map, with the handle inline on each link.
// Handles moved out because they are keyed by a different thing (a GitHub actor
// id, which may be a canonical rather than an alias) and have a different
// lifetime (they expire; a link does not). Legacy files still load — see load.
type storeFile struct {
	Links   map[string]Link   `json:"links"`
	Handles map[string]Handle `json:"handles"`
}

// legacyLink is one entry of the pre-handles-map file format.
type legacyLink struct {
	Canonical string    `json:"canonical"`
	Handle    string    `json:"handle"`
	LinkedAt  time.Time `json:"linkedAt"`
}

// Store is data/identity-links.json: normalized alias actor id → Link, plus the
// GitHub handle cache.
//
// The link map is keyed by the ALIAS because that is the lookup every mint point
// makes ("who is this login?"), and it is what makes one-hop resolution a
// single map read.
type Store struct {
	mu       sync.RWMutex
	filePath string
	links    map[string]Link
	handles  map[string]Handle
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
		handles:  map[string]Handle{},
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
	// Which format this is, decided by structure rather than by a version field
	// we would have had to add retroactively: the legacy file's keys are actor
	// ids, and "links" is not one (it normalizes to "discord:links", which no
	// provider can mint). Guessing wrong in either direction silently drops every
	// binding, which is why this probes instead of trying one and falling back on
	// error — encoding/json ignores unknown fields, so the wrong shape decodes
	// happily into an empty one.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("parse %s: %w", FileName, err)
	}
	var f storeFile
	if _, isNew := probe["links"]; isNew {
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("parse %s: %w", FileName, err)
		}
	} else {
		var legacy map[string]legacyLink
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return fmt.Errorf("parse %s: %w", FileName, err)
		}
		f = migrateLegacy(legacy)
	}
	s.links, s.handles, s.warnings = sanitize(f)
	return nil
}

// migrateLegacy lifts the inline handles of the old format into the handle map.
//
// The link's own timestamp becomes the handle's CheckedAt: at link time the
// handle WAS proved, and it is the only proof instant the old format recorded. A
// link with no timestamp yields an unstamped handle, which reads as unproven —
// under-attributing, the safe direction.
func migrateLegacy(legacy map[string]legacyLink) storeFile {
	f := storeFile{
		Links:   make(map[string]Link, len(legacy)),
		Handles: map[string]Handle{},
	}
	for key, l := range legacy {
		f.Links[key] = Link{Canonical: l.Canonical, LinkedAt: l.LinkedAt}
		if strings.TrimSpace(l.Handle) != "" {
			f.Handles[key] = Handle{Login: l.Handle, CheckedAt: l.LinkedAt}
		}
	}
	return f
}

// sanitize validates a freshly decoded file, returning the links to keep (with
// both sides normalized), the handle cache to keep, and a warning per record
// dropped.
//
// Every rule fails CLOSED — a link it cannot prove unambiguous is dropped, and
// the person falls back to their unlinked identity. Iteration is over sorted
// keys throughout so both the drops and the warnings are deterministic.
func sanitize(f storeFile) (map[string]Link, map[string]Handle, []string) {
	stored := f.Links
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

	return kept, sanitizeHandles(f.Handles, warn), warnings
}

// sanitizeHandles validates the handle cache: keys are GitHub actor ids and the
// login is a non-empty label.
//
// A handle is only a cache, so dropping one costs that person attribution until
// their next GitHub sign-in — never access. Two spellings of one id are dropped
// rather than resolved, on the same reasoning as a duplicated alias: a cache that
// disagrees with itself about who "999" is must not pick a winner.
func sanitizeHandles(stored map[string]Handle, warn func(string, ...any)) map[string]Handle {
	byID := map[string][]string{}
	for _, rawKey := range slices.Sorted(maps.Keys(stored)) {
		id := config.NormalizeActorID(rawKey)
		login := strings.TrimPrefix(strings.TrimSpace(stored[rawKey].Login), "@")
		switch {
		case id == "" || config.ActorKind(id) != config.ActorKindGitHub:
			warn("dropped cached handle keyed %q: only a GitHub actor id has a login.", rawKey)
		case login == "":
			warn("dropped cached handle for %q: it names no login.", rawKey)
		default:
			byID[id] = append(byID[id], rawKey)
		}
	}
	kept := make(map[string]Handle, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		keys := byID[id]
		if len(keys) > 1 {
			warn("dropped %d conflicting cached handles for %q (%s): one GitHub id has one login.",
				len(keys), id, strings.Join(quoteAll(keys), ", "))
			continue
		}
		h := stored[keys[0]]
		h.Login = strings.TrimPrefix(strings.TrimSpace(h.Login), "@")
		kept[id] = h
	}
	return kept
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

// DiscordSubjectFor returns the bare Discord snowflake this account can be
// reached at, whether that is the account's own id or one of its linked logins.
//
// Canonical-at-mint means an account's id is whichever login it was created
// with, so a Google-first person who later linked Discord has a canonical id
// that is not snowflake-shaped — while still being perfectly reachable in
// Discord. Anything that must address a human *in Discord* (a DM, an @mention,
// a reviewer request that pings) has to ask this rather than test the shape of
// the id it happens to hold, or absorb silently removes people from those
// surfaces the moment they link a second login.
//
// ok is false when no Discord login is attached, which is a real state: a
// Google-only user cannot be mentioned in Discord and callers must degrade
// (the inbox, or simply omitting them) rather than invent a target.
func (s *Store) DiscordSubjectFor(canonical string) (string, bool) {
	c := config.NormalizeActorID(canonical)
	if c == "" {
		return "", false
	}
	if config.ActorKind(c) == config.ActorKindDiscord {
		if sub := config.ActorSubject(c); sub != "" {
			return sub, true
		}
	}
	if s == nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, alias := range s.aliasesOfLocked(c) {
		if config.ActorKind(alias) != config.ActorKindDiscord {
			continue
		}
		if sub := config.ActorSubject(alias); sub != "" {
			return sub, true
		}
	}
	return "", false
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

// AliasLink is one linked login, as the account page shows it: the alias in
// wire form plus the metadata the store kept with it.
//
// AliasesOf answers "which logins are this account", which is all the mint and
// notify paths need. The account page additionally has to render *when* a login
// was linked and, for GitHub, under which handle — so this is the one accessor
// that hands out a Link's contents rather than just its key.
type AliasLink struct {
	Alias    string
	Handle   string
	LinkedAt time.Time
	// HandleCheckedAt is when the handle was last proved, and HandleFresh whether
	// that is recent enough for attribution to use it (MaxHandleAge). The page
	// renders a stale handle as stale rather than hiding it: "the name we have is
	// no longer trusted, sign in with GitHub" is actionable, a blank cell is not.
	HandleCheckedAt time.Time
	HandleFresh     bool
}

// LinksOf returns the account's linked logins, ordered like AliasesOf.
func (s *Store) LinksOf(canonical string) []AliasLink {
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
	out := make([]AliasLink, 0, len(aliases))
	for _, alias := range aliases {
		link := s.links[alias]
		h := s.handles[alias]
		out = append(out, AliasLink{
			Alias:           wireForm(alias),
			Handle:          h.Login,
			LinkedAt:        link.LinkedAt,
			HandleCheckedAt: h.CheckedAt,
			HandleFresh:     s.handleFreshLocked(h),
		})
	}
	return out
}

// HandleOf reports the cached GitHub login for one GitHub actor id.
//
// The account page needs this for the row that is the account ITSELF — a
// GitHub-first signup whose canonical id is its GitHub login has no AliasLink to
// read it from. fresh mirrors GitHubFor's own rule, so the page cannot claim
// attribution that attribution would refuse.
func (s *Store) HandleOf(githubActorID string) (login string, checkedAt time.Time, fresh bool) {
	if s == nil {
		return "", time.Time{}, false
	}
	id := config.NormalizeActorID(githubActorID)
	if id == "" || config.ActorKind(id) != config.ActorKindGitHub {
		return "", time.Time{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.handles[id]
	if !ok {
		return "", time.Time{}, false
	}
	return h.Login, h.CheckedAt, s.handleFreshLocked(h)
}

// handleFreshLocked reports whether a cached handle may still be believed.
//
// An unstamped record is NOT fresh: it comes from a file written before proofs
// were recorded, and treating "we never wrote down when we checked" as "we just
// checked" is the whole failure this bound exists to close.
func (s *Store) handleFreshLocked(h Handle) bool {
	if h.Login == "" || h.CheckedAt.IsZero() {
		return false
	}
	return !s.now().After(h.CheckedAt.Add(MaxHandleAge))
}

// GitHubFor returns the GitHub login and numeric id of an account's GitHub login.
//
// The numeric id is that login's own subject (the identity); the login name is
// the cached handle. The account's OWN id is considered first: a GitHub-first
// signup is "github:<id>" and has no alias to find, and walking only the aliases
// left that person permanently unattributable with nothing they could do about it
// — /account offers no button for a provider already on the account, and linking
// your own canonical login back onto itself is refused as a self-link.
//
// ok is false when the account has no GitHub login at all, when the one it has
// has no cached handle, and when that handle is older than MaxHandleAge. All
// three are the same answer on purpose: "<id>+@users.noreply.github.com" is a
// malformed address, and an expired handle may name somebody else now — no
// attribution is better than wrong attribution, which looks like it worked. A
// second GitHub login that does carry a fresh handle is still used.
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
	for _, id := range s.githubLoginsLocked(c) {
		subject := config.ActorSubject(id)
		h := s.handles[id]
		if subject == "" || !s.handleFreshLocked(h) {
			continue
		}
		return h.Login, subject, true
	}
	return "", "", false
}

// githubLoginsLocked lists the GitHub actor ids that are this account: itself
// when it is one, then its GitHub aliases in AliasesOf order.
func (s *Store) githubLoginsLocked(canonical string) []string {
	var out []string
	if config.ActorKind(canonical) == config.ActorKindGitHub {
		out = append(out, canonical)
	}
	for _, alias := range s.aliasesOfLocked(canonical) {
		if config.ActorKind(alias) == config.ActorKindGitHub {
			out = append(out, alias)
		}
	}
	return out
}

// NoreplyEmail builds the git author address for a linked GitHub account.
//
// GitHub's per-account noreply address is the only email we may put in public
// git history. It attributes correctly — GitHub matches the numeric id, so the
// commit lands on the person's profile and contribution graph — without
// publishing a personal address that they never consented to have committed.
// It also cannot go stale the way a cached real email can: the id is immutable,
// and a renamed account keeps working because the id, not the login, is what
// GitHub matches on.
//
// Both halves are required. "<id>+@users.noreply.github.com" and
// "+login@users.noreply.github.com" are malformed addresses that attribute to
// nobody, and a trailer that attributes to nobody is worse than no trailer:
// it looks like the feature worked.
func NoreplyEmail(login, numericID string) string {
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	numericID = strings.TrimSpace(numericID)
	if login == "" || numericID == "" {
		return ""
	}
	return numericID + "+" + login + "@users.noreply.github.com"
}

// RefreshHandle re-caches the GitHub login just proved by a sign-in or a link.
//
// A GitHub login is mutable — the person renames the account, or transfers the
// name — while the numeric id never changes. So the handle is re-proved on every
// GitHub sign-in, not just at link time, or the noreply address and the "@login"
// we write into public history would keep naming a login that now belongs to
// somebody else. The stamp it records is what MaxHandleAge expires, for the
// majority who sign in through some OTHER provider and never come back here.
//
// Deliberately narrow, because this runs on every GitHub login:
//   - it never creates a LINK. Caching "999 is called alice" is a fact GitHub
//     just told us; asserting "999 is this account" is the inference the whole
//     package refuses to make from a sign-in. Only the first happens here — the
//     handle map is keyed by GitHub id and says nothing about ownership.
//   - an unchanged handle re-stamped less than handleRecheckFloor ago writes
//     nothing, so the common path costs one read lock and no disk I/O.
//   - an EMPTY handle is ignored rather than stored. The provider not returning
//     a login is a hiccup, and dropping the cached one would silently turn off
//     that person's attribution (GitHubFor skips a handle-less login).
//
// The caller decides WHOSE handles are worth keeping: web's login callback only
// refreshes once the sign-in is authorized, so the cache is bounded by the
// allowlist instead of by everyone on GitHub who can reach the callback.
func (s *Store) RefreshHandle(githubActorID, handle string) error {
	if s == nil {
		return nil
	}
	a := config.NormalizeActorID(githubActorID)
	handle = strings.TrimPrefix(strings.TrimSpace(handle), "@")
	if a == "" || handle == "" || config.ActorKind(a) != config.ActorKindGitHub {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	prev, existed := s.handles[a]
	if existed && prev.Login == handle && now.Sub(prev.CheckedAt) < handleRecheckFloor {
		return nil
	}
	s.handles[a] = Handle{Login: handle, CheckedAt: now}
	if err := s.save(); err != nil {
		if existed {
			s.handles[a] = prev
		} else {
			delete(s.handles, a)
		}
		return err
	}
	return nil
}

// Link binds alias to canonical, refreshing the cached handle.
//
// Re-linking an alias to the account it already belongs to only refreshes the
// handle (a GitHub sign-in does this too) and keeps the original LinkedAt,
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

	next := Link{Canonical: c, LinkedAt: s.now().UTC()}
	if existed && !prev.LinkedAt.IsZero() {
		next.LinkedAt = prev.LinkedAt
	}
	s.links[a] = next
	// The OAuth round trip that produced this link just proved the handle, so it
	// is stamped now rather than at the (possibly much older) link time.
	prevHandle, hadHandle := s.handles[a]
	if handle != "" {
		s.handles[a] = Handle{Login: handle, CheckedAt: s.now().UTC()}
	}
	if err := s.save(); err != nil {
		// Keep memory and disk agreeing: a link that works until the next
		// restart is worse than one that never took.
		if existed {
			s.links[a] = prev
		} else {
			delete(s.links, a)
		}
		if handle != "" {
			if hadHandle {
				s.handles[a] = prevHandle
			} else {
				delete(s.handles, a)
			}
		}
		return err
	}
	return nil
}

// Unlink removes a login binding. Unlinking something that is not linked is
// not an error — the account page can be submitted twice.
//
// The GitHub handle cached for that login is left alone: it says "999 is called
// alice", which is still true and still that login's own fact, and GitHubFor only
// ever consults the handles of an account and its aliases. Deleting it would also
// throw away the proof stamp of a login that is now an account in its own right.
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
	raw, err := json.MarshalIndent(storeFile{Links: s.links, Handles: s.handles}, "", "  ")
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
