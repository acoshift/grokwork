package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// ProjectErrorsConfig is per-project production error-source opt-in.
type ProjectErrorsConfig struct {
	GCP     *ProjectGCPErrorsConfig     `json:"gcp,omitempty"`
	Sentry  *ProjectSentryConfig        `json:"sentry,omitempty"`
	Deploys *ProjectDeploysErrorsConfig `json:"deploys,omitempty"`
}

// ProjectGCPErrorsConfig is Google Cloud Error Reporting settings.
type ProjectGCPErrorsConfig struct {
	Enabled         bool   `json:"enabled,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
	ProjectNumber   string `json:"projectNumber,omitempty"`
	CredentialsFile string `json:"credentialsFile,omitempty"`
	Service         string `json:"service,omitempty"`
}

// ProjectSentryConfig is Sentry REST consumer settings.
type ProjectSentryConfig struct {
	Enabled   bool   `json:"enabled,omitempty"`
	Org       string `json:"org,omitempty"`
	Project   string `json:"project,omitempty"`
	AuthToken string `json:"authToken,omitempty"`
	BaseURL   string `json:"baseURL,omitempty"`
}

// ProjectDeploysErrorsConfig is deploys.app error.list / error.get / error.update settings.
// This is not grokwork's own deploy pipeline (see ProjectDeployConfig).
type ProjectDeploysErrorsConfig struct {
	Enabled    bool   `json:"enabled,omitempty"`
	Project    string `json:"project,omitempty"`
	Location   string `json:"location,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	APIToken   string `json:"apiToken,omitempty"`
}

func cloneProjectErrors(e *ProjectErrorsConfig) *ProjectErrorsConfig {
	if e == nil {
		return nil
	}
	out := &ProjectErrorsConfig{}
	if e.GCP != nil {
		cp := *e.GCP
		out.GCP = &cp
	}
	if e.Sentry != nil {
		cp := *e.Sentry
		out.Sentry = &cp
	}
	if e.Deploys != nil {
		cp := *e.Deploys
		out.Deploys = &cp
	}
	if out.GCP == nil && out.Sentry == nil && out.Deploys == nil {
		return nil
	}
	return out
}

func normalizeProjectErrors(e *ProjectErrorsConfig) (*ProjectErrorsConfig, error) {
	if e == nil {
		return nil, nil
	}
	if e.GCP != nil {
		g := *e.GCP
		g.ProjectID = strings.TrimSpace(g.ProjectID)
		g.ProjectNumber = strings.TrimSpace(g.ProjectNumber)
		g.CredentialsFile = strings.TrimSpace(g.CredentialsFile)
		g.Service = strings.TrimSpace(g.Service)
		if err := validateErrorsCredentialsFile(g.CredentialsFile); err != nil {
			return nil, err
		}
		if !g.Enabled && g.ProjectID == "" && g.ProjectNumber == "" && g.CredentialsFile == "" && g.Service == "" {
			e.GCP = nil
		} else {
			e.GCP = &g
		}
	}
	if e.Sentry != nil {
		s := *e.Sentry
		s.Org = strings.TrimSpace(s.Org)
		s.Project = strings.TrimSpace(s.Project)
		s.AuthToken = strings.TrimSpace(s.AuthToken)
		s.BaseURL = strings.TrimSpace(s.BaseURL)
		if err := validateSentryBaseURL(s.BaseURL); err != nil {
			return nil, err
		}
		if !s.Enabled && s.Org == "" && s.Project == "" && s.AuthToken == "" && s.BaseURL == "" {
			e.Sentry = nil
		} else {
			e.Sentry = &s
		}
	}
	if e.Deploys != nil {
		d := *e.Deploys
		d.Project = strings.TrimSpace(d.Project)
		d.Location = strings.TrimSpace(d.Location)
		d.Deployment = strings.TrimSpace(d.Deployment)
		d.APIToken = strings.TrimSpace(d.APIToken)
		if !d.Enabled && d.Project == "" && d.Location == "" && d.Deployment == "" && d.APIToken == "" {
			e.Deploys = nil
		} else {
			e.Deploys = &d
		}
	}
	if e.GCP == nil && e.Sentry == nil && e.Deploys == nil {
		return nil, nil
	}
	return e, nil
}

func validateErrorsCredentialsFile(p string) error {
	if p == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("errors gcp credentialsFile must be an absolute path")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return fmt.Errorf("errors gcp credentialsFile must not contain control characters")
		}
	}
	return nil
}

func validateSentryBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("errors sentry baseURL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("errors sentry baseURL must be https")
	}
	if u.User != nil {
		return fmt.Errorf("errors sentry baseURL must not include userinfo")
	}
	if u.Host == "" {
		return fmt.Errorf("errors sentry baseURL host is required")
	}
	if strings.Contains(u.Host, `\`) {
		return fmt.Errorf("errors sentry baseURL host is invalid")
	}
	return nil
}

func sentryAuthTokenFromEnv(project string) string {
	suf := ProjectEnvKeySuffix(project)
	if suf == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv("SENTRY_AUTH_TOKEN_" + suf))
}

func deploysAPITokenFromEnv(project string) string {
	suf := ProjectEnvKeySuffix(project)
	if suf == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv("DEPLOYS_API_TOKEN_" + suf))
}

func deploysBasicFromEnv(project string) (user, pass string) {
	suf := ProjectEnvKeySuffix(project)
	if suf == "" {
		return "", ""
	}
	return strings.TrimSpace(os.Getenv("DEPLOYS_AUTH_USER_" + suf)), strings.TrimSpace(os.Getenv("DEPLOYS_AUTH_PASS_" + suf))
}

// ProjectErrorsAnyEnabled reports whether any error provider is opted in.
func (c *Config) ProjectErrorsAnyEnabled(name string) bool {
	return c.ProjectGCPErrorsEnabled(name) || c.ProjectSentryEnabled(name) || c.ProjectDeploysErrorsEnabled(name)
}

// AnyProjectErrorsEnabled reports whether any project has an error provider on.
func (c *Config) AnyProjectErrorsEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, pc := range c.Projects {
		if errorsAnyEnabled(pc.Errors) {
			return true
		}
	}
	return false
}

func errorsAnyEnabled(e *ProjectErrorsConfig) bool {
	if e == nil {
		return false
	}
	if e.GCP != nil && e.GCP.Enabled {
		return true
	}
	if e.Sentry != nil && e.Sentry.Enabled {
		return true
	}
	if e.Deploys != nil && e.Deploys.Enabled {
		return true
	}
	return false
}

// ProjectGCPErrorsEnabled reports whether GCP Error Reporting is opted in.
func (c *Config) ProjectGCPErrorsEnabled(name string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	return ok && pc.Errors != nil && pc.Errors.GCP != nil && pc.Errors.GCP.Enabled
}

// ProjectSentryEnabled reports whether Sentry is opted in.
func (c *Config) ProjectSentryEnabled(name string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	return ok && pc.Errors != nil && pc.Errors.Sentry != nil && pc.Errors.Sentry.Enabled
}

// ProjectDeploysErrorsEnabled reports whether deploys.app errors are opted in.
func (c *Config) ProjectDeploysErrorsEnabled(name string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	return ok && pc.Errors != nil && pc.Errors.Deploys != nil && pc.Errors.Deploys.Enabled
}

// ProjectGCPErrors returns a clone of the GCP block (path, never key contents).
func (c *Config) ProjectGCPErrors(name string) *ProjectGCPErrorsConfig {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok || pc.Errors == nil || pc.Errors.GCP == nil {
		return nil
	}
	cp := *pc.Errors.GCP
	return &cp
}

// ProjectSentry returns a clone of the Sentry block (token included for the
// live client; Snapshot never exposes it).
func (c *Config) ProjectSentry(name string) *ProjectSentryConfig {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok || pc.Errors == nil || pc.Errors.Sentry == nil {
		return nil
	}
	cp := *pc.Errors.Sentry
	return &cp
}

// ProjectDeploysErrors returns a clone of the deploys.app errors block.
func (c *Config) ProjectDeploysErrors(name string) *ProjectDeploysErrorsConfig {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok || pc.Errors == nil || pc.Errors.Deploys == nil {
		return nil
	}
	cp := *pc.Errors.Deploys
	return &cp
}

// ProjectSentryAuthToken is config token if set, else SENTRY_AUTH_TOKEN_<SUFFIX>.
func (c *Config) ProjectSentryAuthToken(name string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	var fromConfig string
	if pc, ok := c.Projects[name]; ok && pc.Errors != nil && pc.Errors.Sentry != nil {
		fromConfig = strings.TrimSpace(pc.Errors.Sentry.AuthToken)
	}
	c.mu.RUnlock()
	if fromConfig != "" {
		return fromConfig
	}
	return sentryAuthTokenFromEnv(name)
}

// ProjectDeploysAPIToken is config token if set, else DEPLOYS_API_TOKEN_<SUFFIX>.
func (c *Config) ProjectDeploysAPIToken(name string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	var fromConfig string
	if pc, ok := c.Projects[name]; ok && pc.Errors != nil && pc.Errors.Deploys != nil {
		fromConfig = strings.TrimSpace(pc.Errors.Deploys.APIToken)
	}
	c.mu.RUnlock()
	if fromConfig != "" {
		return fromConfig
	}
	return deploysAPITokenFromEnv(name)
}

// ProjectDeploysBasicAuth is DEPLOYS_AUTH_USER_<SUFFIX> + DEPLOYS_AUTH_PASS_<SUFFIX>.
func (c *Config) ProjectDeploysBasicAuth(name string) (user, pass string) {
	return deploysBasicFromEnv(name)
}

// ProjectGCPErrorsCanResolve is enabled and a GCP project id is set (ADC may
// still work with an empty credentialsFile).
func (c *Config) ProjectGCPErrorsCanResolve(name string) bool {
	g := c.ProjectGCPErrors(name)
	return c.ProjectGCPErrorsEnabled(name) && g != nil && strings.TrimSpace(g.ProjectID) != ""
}

// ProjectSentryCanResolve is enabled and a token is available.
func (c *Config) ProjectSentryCanResolve(name string) bool {
	return c.ProjectSentryEnabled(name) && c.ProjectSentryAuthToken(name) != ""
}

// ProjectDeploysErrorsCanResolve is enabled and a Bearer token or Basic pair is available.
func (c *Config) ProjectDeploysErrorsCanResolve(name string) bool {
	if !c.ProjectDeploysErrorsEnabled(name) {
		return false
	}
	if c.ProjectDeploysAPIToken(name) != "" {
		return true
	}
	u, p := c.ProjectDeploysBasicAuth(name)
	return u != "" && p != ""
}

// SetProjectErrorsGCP updates GCP Error Reporting settings and persists.
func (c *Config) SetProjectErrorsGCP(name string, enabled bool, projectID, projectNumber, credentialsFile, service string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	projectID = strings.TrimSpace(projectID)
	projectNumber = strings.TrimSpace(projectNumber)
	credentialsFile = strings.TrimSpace(credentialsFile)
	service = strings.TrimSpace(service)
	if err := validateErrorsCredentialsFile(credentialsFile); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	errs := ensureProjectErrors(pc.Errors)
	g := &ProjectGCPErrorsConfig{}
	if errs.GCP != nil {
		*g = *errs.GCP
	}
	g.Enabled = enabled
	g.ProjectID = projectID
	g.ProjectNumber = projectNumber
	g.CredentialsFile = credentialsFile
	g.Service = service
	if !g.Enabled && g.ProjectID == "" && g.ProjectNumber == "" && g.CredentialsFile == "" && g.Service == "" {
		errs.GCP = nil
	} else {
		errs.GCP = g
	}
	pc.Errors = emptyProjectErrors(errs)
	c.Projects[name] = pc
	return c.saveLocked()
}

// SetProjectErrorsSentry updates Sentry settings and persists.
// authToken empty leaves the stored token; clearToken true clears it.
func (c *Config) SetProjectErrorsSentry(name string, enabled bool, org, project, authToken, baseURL string, clearToken bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	org = strings.TrimSpace(org)
	project = strings.TrimSpace(project)
	authToken = strings.TrimSpace(authToken)
	baseURL = strings.TrimSpace(baseURL)
	if err := validateSentryBaseURL(baseURL); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	errs := ensureProjectErrors(pc.Errors)
	s := &ProjectSentryConfig{}
	if errs.Sentry != nil {
		*s = *errs.Sentry
	}
	s.Enabled = enabled
	s.Org = org
	s.Project = project
	s.BaseURL = baseURL
	if clearToken {
		s.AuthToken = ""
	} else if authToken != "" {
		s.AuthToken = authToken
	}
	if !s.Enabled && s.Org == "" && s.Project == "" && s.AuthToken == "" && s.BaseURL == "" {
		errs.Sentry = nil
	} else {
		errs.Sentry = s
	}
	pc.Errors = emptyProjectErrors(errs)
	c.Projects[name] = pc
	return c.saveLocked()
}

// SetProjectErrorsDeploys updates deploys.app errors settings and persists.
func (c *Config) SetProjectErrorsDeploys(name string, enabled bool, project, location, deployment, apiToken string, clearToken bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	project = strings.TrimSpace(project)
	location = strings.TrimSpace(location)
	deployment = strings.TrimSpace(deployment)
	apiToken = strings.TrimSpace(apiToken)
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	errs := ensureProjectErrors(pc.Errors)
	d := &ProjectDeploysErrorsConfig{}
	if errs.Deploys != nil {
		*d = *errs.Deploys
	}
	d.Enabled = enabled
	d.Project = project
	d.Location = location
	d.Deployment = deployment
	if clearToken {
		d.APIToken = ""
	} else if apiToken != "" {
		d.APIToken = apiToken
	}
	if !d.Enabled && d.Project == "" && d.Location == "" && d.Deployment == "" && d.APIToken == "" {
		errs.Deploys = nil
	} else {
		errs.Deploys = d
	}
	pc.Errors = emptyProjectErrors(errs)
	c.Projects[name] = pc
	return c.saveLocked()
}

func ensureProjectErrors(e *ProjectErrorsConfig) *ProjectErrorsConfig {
	if e == nil {
		return &ProjectErrorsConfig{}
	}
	cp := *e
	if e.GCP != nil {
		g := *e.GCP
		cp.GCP = &g
	}
	if e.Sentry != nil {
		s := *e.Sentry
		cp.Sentry = &s
	}
	if e.Deploys != nil {
		d := *e.Deploys
		cp.Deploys = &d
	}
	return &cp
}

func emptyProjectErrors(e *ProjectErrorsConfig) *ProjectErrorsConfig {
	if e == nil || (e.GCP == nil && e.Sentry == nil && e.Deploys == nil) {
		return nil
	}
	return e
}
