package config

import (
	"fmt"
	"slices"
	"strings"
)

// RewriteActorID moves every grant naming from onto to, and reports how many
// entries changed.
//
// This is the config half of absorbing a linked login (see internal/identity).
// Linking says "these two logins are one person", and from that moment the
// alias is never minted again — so a grant still spelled with it is a grant
// nobody can ever match. Rewriting is what keeps the person's access, teams and
// capability mapping working across the link instead of silently expiring them.
//
// Every list this touches is matched with SameActor, never ==, because an
// operator may have written "discord:123" where the runtime id is "123".
//
// Two properties the caller depends on:
//
//   - Idempotent. Running it twice changes nothing the second time and reports
//     0, so a retried link (the recovery path when a rewrite failed halfway) is
//     safe.
//   - No duplicates. Where the list already names the account, the alias entry
//     is dropped rather than replaced — otherwise absorbing would leave the same
//     person in an allowlist twice, and in a team's member list twice.
//
// Nothing is written when nothing changed: config.json is a live file other
// agents read, and rewriting it to prove a no-op is pure risk.
func (c *Config) RewriteActorID(from, to string) (int, error) {
	if c == nil {
		return 0, fmt.Errorf("config is not loaded")
	}
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return 0, fmt.Errorf("both actor ids are required")
	}
	if SameActor(from, to) {
		// Not an error: the caller may not know the two ids denote one actor,
		// and rewriting an id onto itself is exactly a no-op.
		return 0, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	n := 0
	if c.WebAuth != nil {
		c.WebAuth.AdminDiscordIDs = rewriteIDList(c.WebAuth.AdminDiscordIDs, from, to, &n)
		c.WebAuth.MemberDiscordIDs = rewriteIDList(c.WebAuth.MemberDiscordIDs, from, to, &n)
		c.WebAuth.ViewerDiscordIDs = rewriteIDList(c.WebAuth.ViewerDiscordIDs, from, to, &n)
	}
	for name, pc := range c.Projects {
		changed := false
		if next := rewriteIDList(pc.AllowedUserIDs, from, to, &n); !slices.Equal(next, pc.AllowedUserIDs) {
			pc.AllowedUserIDs = next
			changed = true
		}
		if len(pc.Teams) > 0 {
			teams := cloneTeams(pc.Teams)
			teamsChanged := false
			for key, t := range teams {
				next := rewriteIDList(t.Members, from, to, &n)
				if slices.Equal(next, t.Members) {
					continue
				}
				t.Members = next
				teams[key] = t
				teamsChanged = true
			}
			if teamsChanged {
				pc.Teams = teams
				changed = true
			}
		}
		// capabilityByUser is keyed by actor id. The alias's key is removed
		// either way; its template is carried over only when the account has no
		// mapping of its own, since the account's own capability level is the
		// one already in force for the session doing the linking.
		if len(pc.CapabilityByUser) > 0 {
			var aliasKey, canonicalKey string
			for k := range pc.CapabilityByUser {
				if SameActor(k, from) {
					aliasKey = k
				}
				if SameActor(k, to) {
					canonicalKey = k
				}
			}
			if aliasKey != "" {
				caps := make(map[string]string, len(pc.CapabilityByUser))
				for k, v := range pc.CapabilityByUser {
					caps[k] = v
				}
				template := caps[aliasKey]
				delete(caps, aliasKey)
				if canonicalKey == "" {
					caps[to] = template
				}
				pc.CapabilityByUser = caps
				n++
				changed = true
			}
		}
		if changed {
			c.Projects[name] = pc
		}
	}
	if n == 0 {
		return 0, nil
	}
	if err := c.saveLocked(); err != nil {
		return 0, err
	}
	return n, nil
}

// rewriteIDList replaces from with to in one actor-id list, collapsing onto an
// existing to entry rather than duplicating it. n is incremented per entry
// changed. The input slice is never mutated: the config hands out clones, and a
// Snapshot taken earlier must not change under its reader.
func rewriteIDList(ids []string, from, to string, n *int) []string {
	if len(ids) == 0 || !containsID(ids, from) {
		return ids
	}
	already := containsID(ids, to)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !SameActor(id, from) {
			out = append(out, id)
			continue
		}
		*n++
		if already {
			continue
		}
		out = append(out, to)
		already = true
	}
	return out
}
