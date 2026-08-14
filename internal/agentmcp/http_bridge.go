package agentmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/acoshift/grokwork/internal/agentapi"
)

// maxUnixPath is below Darwin's sockaddr_un sun_path (104) with margin.
const maxUnixPath = 96

// Bridge is an HTTP-over-UDS tool bridge for the host process.
type Bridge struct {
	Service *agentapi.Service
	srv     *http.Server
}

// ListenAndServeUnix serves POST /v1/tool on a Unix socket path.
func (b *Bridge) ListenAndServeUnix(socketPath string) error {
	ln, err := b.listenUnix(socketPath)
	if err != nil {
		return err
	}
	return b.srv.Serve(ln)
}

// ListenUnix binds the socket and serves in the background. Returns after bind.
func (b *Bridge) ListenUnix(socketPath string) error {
	ln, err := b.listenUnix(socketPath)
	if err != nil {
		return err
	}
	go func() { _ = b.srv.Serve(ln) }()
	return nil
}

func (b *Bridge) listenUnix(socketPath string) (net.Listener, error) {
	if b == nil || b.Service == nil {
		return nil, fmt.Errorf("bridge not configured")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tool", b.handleTool)
	b.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	socketPath = ShortUnixPath(socketPath)
	_ = removeSocket(socketPath)
	return net.Listen("unix", socketPath)
}

// ShortUnixPath returns path, or a hashed socket under TempDir when path is
// longer than Darwin's unix-socket limit (test TempDirs routinely overflow).
func ShortUnixPath(path string) string {
	path = filepath.Clean(path)
	if len(path) <= maxUnixPath {
		return path
	}
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(os.TempDir(), "gw-mcp-"+hex.EncodeToString(sum[:8])+".sock")
}

// Shutdown stops the bridge.
func (b *Bridge) Shutdown(ctx context.Context) error {
	if b == nil || b.srv == nil {
		return nil
	}
	return b.srv.Shutdown(ctx)
}

type toolReq struct {
	Token string         `json:"token"`
	Name  string         `json:"name"`
	Args  map[string]any `json:"args"`
}

func (b *Bridge) handleTool(w http.ResponseWriter, r *http.Request) {
	var req toolReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	out, err := Call(r.Context(), b.Service, req.Token, req.Name, req.Args)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
}

// ClientCall dials the UDS bridge and invokes a tool.
func ClientCall(ctx context.Context, socketPath, token, name string, args map[string]any) (any, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 2 * time.Minute}
	body, _ := json.Marshal(toolReq{Token: token, Name: name, Args: args})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/tool", bytesReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Result any    `json:"result"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("%s", out.Error)
	}
	return out.Result, nil
}

func removeSocket(path string) error {
	return removeFile(path)
}
