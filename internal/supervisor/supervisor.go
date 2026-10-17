// Package supervisor owns one mount session: it starts the loopback WebDAV
// endpoint, generates the temporary rclone configuration rclone mounts
// through, and cleans both up when the session ends (issue #18, ADR 0009).
//
// The security boundary is the loopback bind plus the pinned Cookie header —
// there is no second Basic Auth credential to invent, which is exactly why
// the temporary config file is part of the mount safety contract: it carries
// the Quark cookie and must be permission-restricted and deleted.
package supervisor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muou000/quark-rclone/internal/adapter"
	"github.com/muou000/quark-rclone/internal/dav"
	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/session"
)

// defaultDirTTL is how long a DAV directory listing stays cached. Short
// enough that an out-of-band provider change surfaces, long enough that a
// rclone stat burst costs one listing.
const defaultDirTTL = 30 * time.Second

// Config describes one mount session.
type Config struct {
	// Port is the fixed loopback port the endpoint binds. ADR 0009 chooses a
	// fixed port for supervisor and diagnostic predictability, so zero is a
	// configuration error rather than "pick one".
	Port int
	// Cookie is the session's starting credential. It is pinned as the
	// endpoint's bootstrap token and becomes the store's initial value.
	Cookie string
	// Persist receives every rotated cookie so the supervisor can write it to
	// durable storage (DPAPI in a later issue). May be nil.
	Persist func(cookie string)
	// RootFID is the drive directory the DAV root maps to. Empty means the
	// drive root.
	RootFID string
	// APIBaseURL overrides the provider base URL. Tests point it at a local
	// fake; production leaves it empty.
	APIBaseURL string
	// HTTPClient is the wire client's transport. Tests inject one bound to
	// the fake; nil uses the default.
	HTTPClient *http.Client
}

// Session is one running mount session. Stop tears everything down.
type Session struct {
	store      *session.Store
	refresher  *session.Refresher
	server     *dav.Server
	configPath string
	configDir  string
	serveErr   chan error
}

// Start boots the endpoint and writes the temporary rclone config. The
// endpoint is listening when Start returns; rclone can mount immediately.
func Start(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Port <= 0 {
		return nil, fmt.Errorf("supervisor: port must be a fixed positive number, got %d", cfg.Port)
	}
	if cfg.Cookie == "" {
		return nil, fmt.Errorf("supervisor: a mount session needs an active cookie")
	}
	// rclone's headers option splits header pairs on commas, so a cookie that
	// contains one cannot be carried by the temporary config. Better to fail
	// the session at start than to hand rclone a silently broken header.
	if strings.ContainsAny(cfg.Cookie, ",\r\n") {
		return nil, fmt.Errorf("supervisor: cookie contains a character the rclone header format cannot carry")
	}

	store := session.NewStore(cfg.Cookie, cfg.Persist)
	api := quarkapi.New(store, quarkapi.Config{BaseURL: cfg.APIBaseURL}, cfg.HTTPClient)
	svc := adapter.New(api, store)

	server := dav.New(dav.Config{
		Addr:     fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		BasePath: "/dav",
		Cookie:   cfg.Cookie,
		RootFID:  cfg.RootFID,
		Service:  svc,
		DirTTL:   defaultDirTTL,
	})
	if err := server.Listen(); err != nil {
		return nil, err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	configDir, err := os.MkdirTemp("", "quark-rclone-mount-")
	if err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("supervisor: temp dir: %w", err)
	}
	configPath := filepath.Join(configDir, "rclone.conf")
	if err := os.WriteFile(configPath, renderRcloneConfig(cfg.Port, cfg.Cookie), 0o600); err != nil {
		_ = server.Close()
		_ = os.RemoveAll(configDir)
		return nil, fmt.Errorf("supervisor: write rclone config: %w", err)
	}

	refresher := session.NewRefresher(store, api, 0, 0)
	refresher.Start(ctx)

	s := &Session{
		store:      store,
		refresher:  refresher,
		server:     server,
		configPath: configPath,
		configDir:  configDir,
		serveErr:   serveErr,
	}
	return s, nil
}

// Done yields the endpoint's serve error when the listener dies for a reason
// other than Stop. A supervisor watches it: an endpoint that stopped serving
// must end the mount session rather than leave rclone hanging on transfers
// that can never complete.
func (s *Session) Done() <-chan error { return s.serveErr }

// renderRcloneConfig is the temporary configuration's whole content: the
// loopback URL (which carries the DAV root path), the vendor shape rclone
// expects, and the Cookie header. No password exists to leak onto a command
// line — the file is the only place the credential appears.
func renderRcloneConfig(port int, cookie string) []byte {
	return fmt.Appendf(nil,
		"[quark]\ntype = webdav\nvendor = other\nurl = http://127.0.0.1:%d/dav\nheaders = Cookie,%s\n",
		port, cookie)
}

// ConfigPath is where the temporary rclone config lives. The caller passes
// it to rclone via --config and must not copy it elsewhere.
func (s *Session) ConfigPath() string { return s.configPath }

// URL is the loopback base URL the endpoint serves, including the DAV root
// path. It is what the rclone config's url field points at and what
// diagnostics print.
func (s *Session) URL() string { return "http://" + s.server.Addr() + "/dav" }

// CookieSnapshot exposes the live credential for diagnostics.
func (s *Session) CookieSnapshot() string { return s.store.CookieSnapshot() }

// Stop drains the endpoint, stops the keep-alive loop and deletes the
// temporary config. It is safe to call more than once.
func (s *Session) Stop() {
	s.refresher.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.server.Shutdown(ctx)
	// The clean shutdown's ErrServerClosed is not a death; drain it so a
	// Done watcher never mistakes Stop for a crash.
	select {
	case <-s.serveErr:
	default:
	}
	_ = os.RemoveAll(s.configDir)
}
