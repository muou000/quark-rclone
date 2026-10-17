package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/muou000/quark-rclone/internal/adapter"
	"github.com/muou000/quark-rclone/internal/quarkerr"
	"golang.org/x/net/webdav"
)

// davMethods is the method surface this endpoint implements, in the order the
// Allow header advertises it.
var davMethods = []string{
	"OPTIONS", "GET", "HEAD", "PUT", "PROPFIND", "MKCOL", "MOVE", "DELETE", "LOCK", "UNLOCK",
}

// refusedMethods are the WebDAV methods this endpoint must answer with a
// stable unsupported result (issue #18): COPY has no provider operation, and
// PROPPATCH/POST have no meaning for a mount client.
var refusedMethods = map[string]bool{"COPY": true, "PROPPATCH": true, "POST": true}

// passthroughHeaders is the allowlist of CDN response headers a download
// forwards to the loopback client. Everything else — Set-Cookie, signed-URL
// echoes, provider diagnostics — is dropped.
var passthroughHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified",
}

// Config is the loopback endpoint's wiring.
type Config struct {
	// Addr must carry a loopback host. The endpoint never binds a non-loopback
	// interface (ADR 0009); Listen refuses anything else.
	Addr string
	// BasePath is the WebDAV root, e.g. "/dav".
	BasePath string
	// Cookie is the pinned session credential rclone must present on every
	// /dav request. It is the session bootstrap only: upstream requests use
	// the live session store.
	Cookie string
	// RootFID is the drive directory the DAV root maps to. Empty means the
	// drive root.
	RootFID string
	// Service is the adapter facade.
	Service *adapter.Service
	// Timeouts bounds the request phases. Unset fields take production
	// defaults.
	Timeouts Timeouts
	// ReadHeaderTimeout and IdleTimeout tune the HTTP server. Unset fields
	// take production defaults. There is deliberately no global Read or
	// Write timeout: streamed transfers are bounded by the phase timeouts and
	// request cancellation instead.
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	// DirTTL is the directory-listing cache lifetime. Zero takes the
	// production default; a negative keeps listings until a write invalidates
	// them.
	DirTTL time.Duration
}

// Server is one loopback WebDAV endpoint.
type Server struct {
	cfg      Config
	fs       *FS
	handler  *webdav.Handler
	http     *http.Server
	ln       net.Listener
	cdclient *http.Client

	startOnce sync.Once
}

// New builds a Server. Call Listen before Serve.
func New(cfg Config) *Server {
	if cfg.BasePath == "" {
		cfg.BasePath = "/dav"
	}
	cfg.Timeouts = cfg.Timeouts.withDefaults()
	if cfg.ReadHeaderTimeout <= 0 {
		cfg.ReadHeaderTimeout = 15 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 2 * time.Minute
	}
	if cfg.DirTTL == 0 {
		cfg.DirTTL = 30 * time.Second
	}
	s := &Server{cfg: cfg}
	s.fs = NewFS(cfg.Service, cfg.RootFID, cfg.Timeouts, cfg.DirTTL)
	// The CDN client refuses redirects: a signed URL that redirects must not
	// silently drop the Cookie and Referer the signature was minted against.
	// The 3xx passes through to the loopback client instead.
	s.cdclient = &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	s.handler = &webdav.Handler{
		Prefix:     cfg.BasePath,
		FileSystem: s.fs,
		// memLS is the in-memory lock system the If and Lock-Token headers
		// resolve against (issue #18).
		LockSystem: webdav.NewMemLS(),
	}
	s.http = &http.Server{
		Handler:           http.HandlerFunc(s.serve),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
	return s
}

// Listen binds the loopback address and returns immediately. The host part of
// Addr must be a loopback name — this is the endpoint's security boundary,
// so a misconfiguration fails loudly instead of binding 0.0.0.0.
func (s *Server) Listen() error {
	host, _, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("dav: invalid addr %q: %w", s.cfg.Addr, err)
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
	default:
		return fmt.Errorf("dav: refusing to bind non-loopback host %q (ADR 0009)", host)
	}
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("dav: listen %s: %w", s.cfg.Addr, err)
	}
	s.ln = ln
	return nil
}

// Serve accepts connections until Shutdown or Close. It returns
// http.ErrServerClosed on a clean shutdown.
func (s *Server) Serve() error {
	if s.ln == nil {
		return errors.New("dav: Serve before Listen")
	}
	return s.http.Serve(s.ln)
}

// Addr returns the bound address, which is how a caller learns the ephemeral
// port after Listen on ":0"-style addresses. The host is normalised to
// 127.0.0.1 for the generated rclone config.
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.cfg.Addr
	}
	host, port, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		return s.cfg.Addr
	}
	if host == "::" || host == "[::]" || host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// Shutdown stops accepting new requests and drains the in-flight ones before
// returning, which is the endpoint's half of the shutdown contract (issue
// #18): rclone may finish whatever transfer it is inside.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// Close closes the listener and all in-flight connections immediately. It is
// the abort path; Shutdown is the clean one.
func (s *Server) Close() error { return s.http.Close() }

// serve is the top-level handler: fixed routes first, the gated WebDAV
// surface second.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Health carries no credential and no capability detail: it exists so
		// the supervisor can tell "process up" from "session usable".
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
		return
	}
	if r.URL.Path == "/capabilities" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveCapabilities(w)
		return
	}
	if !strings.HasPrefix(r.URL.Path, s.cfg.BasePath+"/") && r.URL.Path != s.cfg.BasePath {
		http.NotFound(w, r)
		return
	}
	s.serveDav(w, r)
}

// serveCapabilities reports the backend capability statement plus the DAV
// method surface. Responses contain no credential material.
func (s *Server) serveCapabilities(w http.ResponseWriter) {
	caps := adapter.CapabilitiesOf()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"copy":%t,"overwrite_upload":%t,"base_path":%q,"methods":[`,
		caps.Copy, caps.OverwriteUpload, s.cfg.BasePath)
	for i, m := range davMethods {
		if i > 0 {
			_, _ = w.Write([]byte(","))
		}
		fmt.Fprintf(w, "%q", m)
	}
	_, _ = w.Write([]byte("]}\n"))
}

// serveDav is the gated WebDAV surface. The order matters: the cookie gate
// runs before anything about the request is interpreted, the method gate
// answers unsupported verbs before the webdav handler could give them its own
// meaning, and downloads bypass the handler entirely.
func (s *Server) serveDav(w http.ResponseWriter, r *http.Request) {
	if !s.cookieOK(r) {
		writeDAVError(w, http.StatusUnauthorized, quarkerr.CatAuthRequired,
			"missing or invalid Cookie header")
		return
	}
	if refusedMethods[r.Method] {
		w.Header().Set("Allow", strings.Join(davMethods, ", "))
		writeDAVError(w, http.StatusMethodNotAllowed, quarkerr.CatNotSupported,
			strings.ToLower(r.Method)+" is not supported by the Quark mount endpoint")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.serveDownload(w, r)
	case http.MethodPut:
		s.servePut(w, r)
	default:
		s.handler.ServeHTTP(w, r)
	}
}

// cookieOK reports whether the request presents the session's pinned
// credential. The comparison is constant-time because the cookie is itself a
// bearer credential.
func (s *Server) cookieOK(r *http.Request) bool {
	got := r.Header.Get("Cookie")
	if got == "" || s.cfg.Cookie == "" {
		return false
	}
	return subtleCompare(got, s.cfg.Cookie)
}

// servePut runs the webdav handler with the response buffered so a real
// upload failure can rewrite the generic 405 the handler maps Close errors
// to, into the provider's category status — 409 for a name conflict, 502-class
// for transient upstream failures that rclone should retry.
func (s *Server) servePut(w http.ResponseWriter, r *http.Request) {
	holder := &uploadErrHolder{}
	rw := &bufferedResponse{ResponseWriter: w}
	s.handler.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), uploadErrKey{}, holder)))
	status, body := rw.render()
	if err := holder.get(); err != nil && status >= 400 {
		status = quarkerr.StatusCode(err)
		body = statusText(status)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// serveDownload proxies GET and HEAD of a file through to the CDN URL minted
// for its FID, replaying exactly the headers the signature was minted
// against and passing the client's Range semantics through untouched.
//
// The proxy exists because handing the client the CDN URL would leak the
// credential-bound address and force the client to replay the Cookie itself;
// the loopback contract keeps the client credential-free (issue #18).
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := cleanPath(strings.TrimPrefix(r.URL.Path, s.cfg.BasePath))

	e, err := s.fs.resolve(ctx, p)
	if err != nil {
		writeStatusErr(w, err)
		return
	}
	if e.isDir {
		// Collections have no body; rclone lists them with PROPFIND.
		w.Header().Set("Allow", strings.Join(davMethods, ", "))
		writeDAVError(w, http.StatusMethodNotAllowed, quarkerr.CatNotSupported,
			"get on a collection is not supported")
		return
	}

	mctx, cancelMint := context.WithTimeout(ctx, s.cfg.Timeouts.Mint)
	res, err := s.cfg.Service.DownloadURL(mctx, e.fid)
	cancelMint()
	if err != nil {
		writeStatusErr(w, err)
		return
	}

	sctx := ctx
	if s.cfg.Timeouts.Stream > 0 {
		var cancelStream context.CancelFunc
		sctx, cancelStream = context.WithTimeout(ctx, s.cfg.Timeouts.Stream)
		defer cancelStream()
	}
	up, err := http.NewRequestWithContext(sctx, http.MethodGet, res.URL, nil)
	if err != nil {
		writeStatusErr(w, err)
		return
	}
	// The minted URL is signed against exactly this trio; replay it verbatim.
	for k, v := range res.Header {
		up.Header.Set(k, v)
	}
	if rng := r.Header.Get("Range"); rng != "" {
		up.Header.Set("Range", rng)
	}
	resp, err := s.cdclient.Do(up)
	if err != nil {
		// A client-gone error must not turn into a written status: the
		// requester is already gone.
		if ctx.Err() != nil {
			return
		}
		writeStatusErr(w, err)
		return
	}
	defer resp.Body.Close()

	for _, k := range passthroughHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		// Headers only: the body is dropped, not read.
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

// writeDAVError answers with the stable JSON error shape of this endpoint:
// a quarkerr category and a message this package authored, so no provider
// text and no credential ever reach the wire.
func writeDAVError(w http.ResponseWriter, status int, cat quarkerr.Category, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, "{\"error\":%q,\"message\":%q}\n", string(cat), detail)
}

// writeStatusErr maps an error onto a status via the shared category mapping
// and answers with the same stable shape. Filesystem-shaped errors are mapped
// first: a missing path is 404 no matter which layer produced it.
func writeStatusErr(w http.ResponseWriter, err error) {
	if os.IsNotExist(err) {
		writeDAVError(w, http.StatusNotFound, quarkerr.CatObjectNotFound, statusText(http.StatusNotFound))
		return
	}
	writeDAVError(w, quarkerr.StatusCode(err), quarkerr.CategoryOf(err), statusText(quarkerr.StatusCode(err)))
}

// statusText is the loopback client's rendering of a status, kept static so
// responses are diffable and never carry provider text.
func statusText(status int) string {
	return fmt.Sprintf("quark dav: %s", http.StatusText(status))
}

// uploadErrKey is the context key the PUT path uses to hand the terminal
// upload error to the status fixer.
type uploadErrKey struct{}

// uploadErrHolder carries one terminal error from writeFile.Close to the PUT
// middleware.
type uploadErrHolder struct {
	mu  sync.Mutex
	err error
}

func (h *uploadErrHolder) set(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = err
	}
}

func (h *uploadErrHolder) get() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// subtleCompare is a constant-time string equality for bearer-style secrets.
func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// bufferedResponse buffers one handler response so the PUT status fixer can
// substitute the status after the webdav handler returned. Only PUT uses it,
// and a PUT response body is at most the handler's static status text, so the
// buffer is bounded.
type bufferedResponse struct {
	http.ResponseWriter
	status int
	body   strings.Builder
	header http.Header
}

func (b *bufferedResponse) Header() http.Header {
	if b.header == nil {
		b.header = http.Header{}
	}
	return b.header
}

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

// render returns the buffered status and body, defaulting like net/http does
// when the handler never wrote anything.
func (b *bufferedResponse) render() (int, string) {
	if b.status == 0 {
		return http.StatusOK, ""
	}
	return b.status, b.body.String()
}
