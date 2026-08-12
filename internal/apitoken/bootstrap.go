package apitoken

import (
	"fmt"
	"os"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
)

const (
	envBootstrapToken    = "GROK_WORK_API_BOOTSTRAP_TOKEN"
	envBootstrapProjects = "GROK_WORK_API_BOOTSTRAP_PROJECTS"
)

// BootstrapFromEnv inserts the first token from GROK_WORK_API_BOOTSTRAP_TOKEN
// only when that publicId is absent entirely (revoked rows block re-insert).
func (s *Store) BootstrapFromEnv() (inserted bool, err error) {
	if s == nil {
		return false, nil
	}
	wire := strings.TrimSpace(os.Getenv(envBootstrapToken))
	if wire == "" {
		return false, nil
	}
	return s.bootstrap(wire, os.Getenv(envBootstrapProjects))
}

func (s *Store) bootstrap(wire, projectsCSV string) (bool, error) {
	id, _, ok := parseWire(wire)
	if !ok {
		return false, fmt.Errorf("invalid %s format", envBootstrapToken)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tokens[id]; exists {
		return false, nil
	}
	now := s.now().UTC()
	rec := Record{
		ID:        id,
		Label:     "bootstrap",
		TokenHash: sha256Hex(wire),
		ActorID:   config.NormalizeActorID("token:" + id),
		Projects:  compactProjects(strings.Split(projectsCSV, ",")),
		Caps:      CapsMask{StartSessions: true},
		CreatedAt: now,
		CreatedBy: "bootstrap",
	}
	s.tokens[id] = rec
	if err := s.saveLocked(); err != nil {
		delete(s.tokens, id)
		return false, err
	}
	return true, nil
}
