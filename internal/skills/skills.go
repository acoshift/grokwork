// Package skills discovers coding-agent skill packages on the host.
//
// Locations match Grok's discovery (see ~/.grok/docs/user-guide/08-skills.md)
// and Claude's ~/.claude/skills plus installed-plugin layout so the web UI
// can show what headless runs are likely to load — without shelling out
// to `grok inspect`.
package skills

import (
	"bufio"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Info is one discovered skill definition.
type Info struct {
	Name        string // skill id (frontmatter name or directory stem)
	Description string // one-line description from frontmatter / first paragraph
	Source      string // stable label: "user (grok)", "user (claude)", "bundled", "project:app", …
	Path        string // absolute path to SKILL.md (or command .md)
	// RealPath is Path after EvalSymlinks when resolvable; used to collapse
	// duplicate installs that are the same file via different roots.
	RealPath string
}

// ListOpts configures discovery. Empty Home uses os.UserHomeDir.
type ListOpts struct {
	Home string
	// Projects maps project name → absolute checkout path. Each path is
	// scanned for repo-local skills (.grok/skills, .agents/skills, .claude/skills).
	Projects map[string]string
}

// List returns discovered skills, sorted by name then source.
// Missing roots are skipped (not an error). Individual unreadable skill
// files are omitted rather than failing the whole list.
func List(opts ListOpts) []Info {
	home := strings.TrimSpace(opts.Home)
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return nil
		}
		home = h
	}

	var out []Info
	seenReal := map[string]struct{}{}

	addRoot := func(root, source string) {
		for _, sk := range scanSkillRoot(root, source) {
			key := sk.RealPath
			if key == "" {
				key = sk.Path
			}
			if key != "" {
				if _, ok := seenReal[key]; ok {
					continue
				}
				seenReal[key] = struct{}{}
			}
			out = append(out, sk)
		}
	}

	// User / host roots (lowest priority first so higher-priority labels win
	// if we ever stop collapsing by real path — today first-write keeps one).
	addRoot(filepath.Join(home, ".grok", "bundled", "skills"), "bundled (grok)")
	addRoot(filepath.Join(home, ".cursor", "skills"), "user (cursor)")
	addRoot(filepath.Join(home, ".claude", "skills"), "user (claude)")
	addRoot(filepath.Join(home, ".grok", "skills"), "user (grok)")

	// Extra dirs from ~/.grok/config.toml [skills].paths (best-effort).
	for _, p := range configSkillPaths(filepath.Join(home, ".grok", "config.toml")) {
		addRoot(expandHome(p, home), "config path")
	}

	// Claude Code plugins (and Grok's Claude-compat loader) read
	// ~/.claude/plugins/installed_plugins.json — not the marketplace
	// clones under …/plugins/marketplaces/. Each installPath/skills is
	// one plugin's skill root.
	for _, p := range claudeInstalledPluginSkills(home) {
		addRoot(p.root, p.source)
	}

	// Project-local roots: higher priority conceptually; still collapsed by realpath.
	names := make([]string, 0, len(opts.Projects))
	for name := range opts.Projects {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		projPath := strings.TrimSpace(opts.Projects[name])
		if projPath == "" {
			continue
		}
		label := "project:" + name
		addRoot(filepath.Join(projPath, ".grok", "skills"), label)
		addRoot(filepath.Join(projPath, ".agents", "skills"), label)
		addRoot(filepath.Join(projPath, ".claude", "skills"), label)
		addRoot(filepath.Join(projPath, ".cursor", "skills"), label)
	}

	slices.SortFunc(out, func(a, b Info) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Source, b.Source)
	})
	return out
}

type pluginSkillRoot struct {
	root   string
	source string
}

// claudeInstalledPluginSkills reads Claude's installed-plugin index and
// returns each plugin's skills/ directory. Missing or unreadable JSON is
// skipped (same as a missing skill root). Marketplace clones that are
// not in the index are not scanned.
func claudeInstalledPluginSkills(home string) []pluginSkillRoot {
	data, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var file struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(data, &file) != nil || len(file.Plugins) == 0 {
		return nil
	}
	var out []pluginSkillRoot
	for _, key := range slices.Sorted(maps.Keys(file.Plugins)) {
		name, _, _ := strings.Cut(key, "@")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		source := "plugin:" + name
		for _, inst := range file.Plugins[key] {
			p := expandHome(inst.InstallPath, home)
			if p == "" {
				continue
			}
			out = append(out, pluginSkillRoot{
				root:   filepath.Join(p, "skills"),
				source: source,
			})
		}
	}
	return out
}

func expandHome(p, home string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// scanSkillRoot walks one skill root. root may be a directory of skill dirs,
// or (for [skills].paths) a single SKILL.md file.
func scanSkillRoot(root, source string) []Info {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil
	}
	if !st.IsDir() {
		if strings.EqualFold(filepath.Base(root), "SKILL.md") || strings.HasSuffix(strings.ToLower(root), ".md") {
			if sk, ok := readSkillFile(root, source, ""); ok {
				return []Info{sk}
			}
		}
		return nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Info
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(root, name)
		// Skill directory with SKILL.md
		if e.IsDir() || (e.Type()&os.ModeSymlink) != 0 {
			// Symlink may point at a dir; try SKILL.md inside.
			skillMD := filepath.Join(full, "SKILL.md")
			if sk, ok := readSkillFile(skillMD, source, name); ok {
				out = append(out, sk)
				continue
			}
			// Non-dir entries fall through when Stat fails.
			if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
				continue
			}
			continue
		}
		// Flat *.md under commands-style roots (name = stem).
		if strings.HasSuffix(strings.ToLower(name), ".md") {
			stem := strings.TrimSuffix(name, filepath.Ext(name))
			if sk, ok := readSkillFile(full, source, stem); ok {
				out = append(out, sk)
			}
		}
	}
	return out
}

func readSkillFile(path, source, dirName string) (Info, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Info{}, false
	}
	name, desc := parseFrontmatter(string(data))
	if name == "" {
		name = strings.TrimSpace(dirName)
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if name == "" || strings.EqualFold(name, "SKILL") {
		return Info{}, false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	real := abs
	if r, err := filepath.EvalSymlinks(abs); err == nil && r != "" {
		real = r
	}
	return Info{
		Name:        name,
		Description: desc,
		Source:      source,
		Path:        abs,
		RealPath:    real,
	}, true
}

// parseFrontmatter extracts name and description from YAML frontmatter when
// present; otherwise uses the first non-empty markdown paragraph as description.
func parseFrontmatter(body string) (name, desc string) {
	body = strings.TrimPrefix(body, "\ufeff")
	if !strings.HasPrefix(body, "---") {
		return "", firstParagraph(body)
	}
	rest := body[3:]
	// Allow --- immediately followed by newline.
	if i := strings.Index(rest, "\n"); i >= 0 {
		rest = rest[i+1:]
	} else {
		return "", firstParagraph(body)
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", firstParagraph(body)
	}
	fm := rest[:end]
	sc := bufio.NewScanner(strings.NewReader(fm))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		switch key {
		case "name":
			if name == "" {
				name = val
			}
		case "description":
			if desc == "" {
				desc = val
			}
		}
	}
	if desc == "" {
		desc = firstParagraph(rest[end+4:]) // after closing ---
	}
	return name, truncate(desc, 240)
}

func firstParagraph(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if b.Len() > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			if b.Len() > 0 {
				break
			}
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(line)
	}
	return truncate(b.String(), 240)
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n <= 0 || len(s) <= n {
		return s
	}
	// Avoid cutting mid-rune.
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// configSkillPaths reads [skills].paths from a Grok config.toml without a
// full TOML dependency. Unknown formats yield an empty list.
func configSkillPaths(configPath string) []string {
	f, err := os.Open(configPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	inSkills := false
	var paths []string
	sc := bufio.NewScanner(f)
	// Large enough for a long paths array line.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inSkills = line == "[skills]" || strings.HasPrefix(line, "[skills.")
			// Only the bare [skills] table holds paths; [skills.something] ends it.
			if line != "[skills]" {
				inSkills = false
			}
			continue
		}
		if !inSkills {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) != "paths" {
			continue
		}
		paths = append(paths, parseTOMLStringArray(val)...)
	}
	return paths
}

// parseTOMLStringArray parses a minimal `["a", "b"]` or `['a']` value.
func parseTOMLStringArray(val string) []string {
	val = strings.TrimSpace(val)
	if !strings.HasPrefix(val, "[") {
		// Single string?
		s := strings.Trim(val, `"' `)
		if s != "" {
			return []string{s}
		}
		return nil
	}
	var out []string
	rest := val
	for {
		i := strings.IndexAny(rest, `"'`)
		if i < 0 {
			break
		}
		quote := rest[i]
		rest = rest[i+1:]
		j := strings.IndexByte(rest, quote)
		if j < 0 {
			break
		}
		out = append(out, rest[:j])
		rest = rest[j+1:]
	}
	return out
}
