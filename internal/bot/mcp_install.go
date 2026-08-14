package bot

import (
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/acoshift/grokwork/internal/grokrun"
)

// EnsureGrokworkMCPInstall registers the grokwork stdio MCP server in the
// operator's user-scope grok config so grok (no --mcp-config) can attach.
// Env values are ${} interpolations — never the ClickUp key or a minted token.
func (b *Bot) EnsureGrokworkMCPInstall() {
	if b == nil || b.cfg == nil || !b.cfg.AgentMCPEnabled() {
		return
	}
	exe, err := os.Executable()
	if err != nil || strings.TrimSpace(exe) == "" {
		log.Printf("warn: grok mcp install: executable: %v", err)
		return
	}
	bin := strings.TrimSpace(b.cfg.GrokBin)
	if bin == "" {
		bin = "grok"
	}
	cmd := exec.Command(bin, "mcp", "add", "grokwork",
		"-e", grokrun.AgentTokenEnv+"=${"+grokrun.AgentTokenEnv+"}",
		"-e", grokrun.AgentSockEnv+"=${"+grokrun.AgentSockEnv+"}",
		"--", exe, "agent-mcp-stdio",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("warn: grok mcp add grokwork: %v: %s", err, truncate(string(out), 200))
		return
	}
	log.Printf("agent mcp: ensured user-scope grokwork server")
}
