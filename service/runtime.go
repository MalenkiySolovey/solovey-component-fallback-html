//go:build !minimal

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
	"github.com/MalenkiySolovey/solovey-host/componenthost/publicsurface"
)

const (
	publicSiteRateWindow = time.Minute
	publicSiteRateLimit  = 240
	publicSiteRateMaxKey = 8192
)

type Runtime struct {
	mu         sync.Mutex
	generation atomic.Uint64
	source     PublicationSource
	unregister func()
	serving    HTTPServing
	surface    PublicSurface
	stopped    bool
	draining   atomic.Bool
	workMu     sync.Mutex
	active     uint64
	idle       chan struct{}
	limiter    RequestLimiter
	snapshot   atomic.Value // *snapshot
}

type RuntimeStatus struct {
	Active    bool `json:"active"`
	SiteID    uint `json:"siteId,omitempty"`
	Pages     int  `json:"pages"`
	Redirects int  `json:"redirects"`
}

type snapshot struct {
	state       PublicationState
	targetKey   string
	siteID      uint
	pages       map[string]publishedFile
	redirects   map[string]publishedRedirect
	csp         string
	notFound    publishedFile
	hasNotFound bool
}

type publishedFile struct {
	publicPath string
	filePath   string
	mimeType   string
	sha256     string
	cache      string
	modified   int64
	data       []byte
}

type publishedRedirect struct {
	fromPath   string
	toPath     string
	statusCode int
	external   bool
}

var ErrRuntimeDraining = errors.New("fallback-html runtime must finish draining before activation")

const runtimeDrainTimeout = 5 * time.Second

func (r *Runtime) Start(source PublicationSource) error {
	return r.StartContext(context.Background(), source)
}

func (r *Runtime) StartContext(ctx context.Context, source PublicationSource) error {
	if err := r.lockContext(ctx); err != nil {
		return err
	}
	defer r.mu.Unlock()
	if r.draining.Load() && !r.stopped {
		return ErrRuntimeDraining
	}
	wasStopped := r.stopped
	r.draining.Store(false)
	registered := r.unregister == nil
	if registered {
		unregister, err := r.surface.RegisterPublic(r.ServePublic)
		if err != nil {
			r.draining.Store(wasStopped)
			return err
		}
		r.unregister = unregister
	}
	if err := r.rebuildLocked(ctx, source); err != nil {
		if registered {
			r.unregister()
			r.unregister = nil
		}
		r.draining.Store(wasStopped)
		return err
	}
	r.source = source
	r.stopped = false
	return nil
}

// Stop has a bounded default for legacy callers. Lifecycle owners pass their
// own cancellation deadline through Shutdown and must propagate its result.
func (r *Runtime) Stop() error { return r.Shutdown(context.Background()) }

// lockContext bounds contention with a publication or restore already using
// the same runtime; cancellation never leaves a background Stop running.
func (r *Runtime) lockContext(ctx context.Context) error { return acquireRuntimeLock(ctx, &r.mu) }

func acquireRuntimeLock(ctx context.Context, mu *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for !mu.TryLock() {
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		mu.Unlock()
		return err
	}
	return nil
}

// beginRequest pins immutable bytes until the response and its observation
// finish. Admission and the idle signal share a lock with the drain gate.
func (r *Runtime) beginRequest() (*snapshot, func(), bool) {
	r.workMu.Lock()
	defer r.workMu.Unlock()
	if r.draining.Load() {
		return nil, nil, false
	}
	snap := r.snapshot.Load().(*snapshot)
	if snap == nil {
		return nil, nil, false
	}
	if r.active == 0 {
		r.idle = make(chan struct{})
	}
	r.active++
	return snap, func() {
		r.workMu.Lock()
		defer r.workMu.Unlock()
		r.active--
		if r.active == 0 {
			close(r.idle)
		}
	}, true
}

func (r *Runtime) Rebuild(source PublicationSource) error {
	return r.RebuildContext(context.Background(), source)
}
func (r *Runtime) RebuildContext(ctx context.Context, source PublicationSource) error {
	if err := r.lockContext(ctx); err != nil {
		return err
	}
	defer r.mu.Unlock()
	if r.draining.Load() {
		return ErrRuntimeDraining
	}
	if source != nil {
		r.source = source
	}
	return r.rebuildLocked(ctx, r.source)
}

func (r *Runtime) Status() RuntimeStatus {
	snap := r.snapshot.Load().(*snapshot)
	if snap == nil {
		return RuntimeStatus{}
	}
	return RuntimeStatus{
		Active:    true,
		SiteID:    snap.siteID,
		Pages:     len(snap.pages),
		Redirects: len(snap.redirects),
	}
}

func (r *Runtime) rebuildLocked(ctx context.Context, source PublicationSource) error {
	if source == nil {
		if err := r.serving.Drain(ctx); err != nil {
			return err
		}
		r.snapshot.Store((*snapshot)(nil))
		return nil
	}
	candidate, err := source.LoadPublication(ctx, r.generation.Load())
	if err != nil {
		return err
	}
	if candidate == nil {
		if err := r.serving.Drain(ctx); err != nil {
			return err
		}
		r.snapshot.Store((*snapshot)(nil))
		return nil
	}
	publish := candidate.Publish
	if len(candidate.Files) != len(publish.Files) {
		return errors.New("fallback-html publication file set is incomplete")
	}
	next := &snapshot{
		siteID:    publish.SiteID,
		pages:     make(map[string]publishedFile, len(publish.Files)),
		redirects: make(map[string]publishedRedirect, len(publish.Redirects)),
	}
	next.csp = publicSiteCSP(candidate.Resources)
	for index, file := range publish.Files {
		data := append([]byte(nil), candidate.Files[index]...)
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != file.Sha256 {
			return fmt.Errorf("fallback-html publish file %s failed integrity check", file.PublicPath)
		}
		item := publishedFile{
			publicPath: file.PublicPath,
			filePath:   file.FilePath,
			mimeType:   file.MimeType,
			sha256:     file.Sha256,
			cache:      file.CachePolicy,
			modified:   publish.CreatedAt,
			data:       data,
		}
		if file.PublicPath == "/404.html" {
			next.notFound = item
			next.hasNotFound = true
			continue
		}
		next.pages[file.PublicPath] = item
	}
	for _, redirect := range publish.Redirects {
		next.redirects[redirect.FromPath] = publishedRedirect{
			fromPath:   redirect.FromPath,
			toPath:     redirect.ToPath,
			statusCode: redirect.StatusCode,
			external:   redirect.External,
		}
	}
	targets := candidate.Targets
	binding := candidate.Binding
	if binding == nil {
		return errors.New("fallback-html publication binding is unavailable")
	}
	scope := HTTPBindingScope{Resource: ConfiguredTargetIdentity(publish.SiteID, fallbackdomain.RuntimeTarget{}), PublicationRevision: publish.Version, ExpectedGeneration: r.generation.Load()}
	if len(targets) == 1 {
		scope.Resource = ConfiguredTargetIdentity(publish.SiteID, targets[0])
	}
	if binding.Scope() != scope {
		return errors.New("fallback-html publication binding does not match its candidate")
	}
	if err := binding.Bind(ctx, scope); err != nil {
		return err
	}
	next.state.Generation = r.generation.Add(1)
	next.state.PublicationRevision = publish.Version
	if len(targets) == 1 {
		next.state.Resource = ConfiguredTargetIdentity(publish.SiteID, targets[0])
		next.targetKey = RuntimeTargetKey(targets[0].Listen, targets[0].Port)
	} else {
		// Shared-listener publication still identifies its site; no standalone
		// listener observation is inferred from the configured panel endpoint.
		next.state.Resource = ConfiguredTargetIdentity(publish.SiteID, fallbackdomain.RuntimeTarget{})
	}
	r.snapshot.Store(next)
	return nil
}

func (r *Runtime) Owns(listen string, port int) bool {
	return !r.draining.Load() && r.serving.Owns(listen, port)
}

func RuntimeTargetKey(listen string, port int) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		listen = "127.0.0.1"
	}
	return net.JoinHostPort(listen, strconv.Itoa(port))
}

func (r *Runtime) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	snap, done, ok := r.beginRequest()
	if !ok {
		http.NotFound(w, req)
		return
	}
	defer done()
	started := time.Now()
	observed := &observationResponseWriter{ResponseWriter: w}
	r.serveHTTPPublicResponse(observed, req, snap)
	r.emitHTTPObservation(req, observed.statusCode(), observed.bytes, snap.siteID, time.Since(started))
}

func (r *Runtime) serveHTTPPublicResponse(w http.ResponseWriter, req *http.Request, snap *snapshot) {
	if !r.allowHTTPPublicRequest(w, req, snap.csp) {
		return
	}
	requestPath := req.URL.Path
	if requestPath == "" {
		requestPath = "/"
	}
	path, err := fallbackdomain.NormalizePublicPath(requestPath)
	if err != nil || fallbackdomain.IsReservedPublicPath(path, nil) {
		http.NotFound(w, req)
		return
	}
	if req.Method == http.MethodPost && path == "/" {
		if file, ok := snap.pages["/"]; ok {
			r.writeHTTPFile(w, req, file, http.StatusOK, snap.csp)
			return
		}
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if redirect, ok := snap.redirects[path]; ok {
		setPublicHTTPHeaders(w, snap.csp)
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, req, redirect.toPath, redirect.statusCode)
		return
	}
	if requestPath != path {
		if _, ok := snap.pages[path]; ok {
			setPublicHTTPHeaders(w, snap.csp)
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, req, path, http.StatusPermanentRedirect)
			return
		}
	}
	file, ok := snap.pages[path]
	if !ok {
		if !snap.hasNotFound {
			http.NotFound(w, req)
			return
		}
		r.writeHTTPFile(w, req, snap.notFound, http.StatusNotFound, snap.csp)
		return
	}
	r.writeHTTPFile(w, req, file, http.StatusOK, snap.csp)
}

func (r *Runtime) allowHTTPPublicRequest(w http.ResponseWriter, req *http.Request, csp string) bool {
	if r.limiter == nil {
		return true
	}
	key := req.RemoteAddr
	if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil && host != "" {
		key = host
	}
	if key == "" {
		key = "unknown"
	}
	decision := r.limiter.Allow(key)
	if decision.Allowed {
		return true
	}
	setPublicHTTPHeaders(w, csp)
	retryAfter := int((decision.RetryAfter + time.Second - 1) / time.Second)
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.WriteHeader(http.StatusTooManyRequests)
	return false
}

func (r *Runtime) writeHTTPFile(w http.ResponseWriter, req *http.Request, file publishedFile, status int, csp string) {
	setPublicHTTPHeaders(w, csp)
	if file.cache != "" {
		w.Header().Set("Cache-Control", file.cache)
	} else {
		w.Header().Set("Cache-Control", htmlCachePolicy)
	}
	etag := `"` + file.sha256 + `"`
	w.Header().Set("ETag", etag)
	if file.modified > 0 {
		w.Header().Set("Last-Modified", time.Unix(file.modified, 0).UTC().Format(http.TimeFormat))
	}
	if status == http.StatusOK && requestCacheFresh(req, etag, file.modified) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", file.mimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(file.data)))
	w.WriteHeader(status)
	if req.Method != http.MethodHead {
		_, _ = w.Write(file.data)
	}
}

func setPublicHTTPHeaders(w http.ResponseWriter, csp string) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Del("X-Frame-Options")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Security-Policy", csp)
}

func (r *Runtime) ServePublic(w http.ResponseWriter, req *http.Request, ctx publicsurface.Context, sourceIP string) bool {
	snap, done, ok := r.beginRequest()
	if !ok {
		return false
	}
	defer done()
	started := time.Now()
	observed := &observationResponseWriter{ResponseWriter: w}
	handled := r.servePublicSnapshot(observed, req, ctx, sourceIP, true, snap)
	if handled {
		r.emitPublicObservation(req, sourceIP, observed.statusCode(), observed.bytes, snap.siteID, time.Since(started))
	}
	return handled
}
func (r *Runtime) servePublic(w http.ResponseWriter, req *http.Request, ctx publicsurface.Context, sourceIP string, enforceRateLimit bool) bool {
	if r.draining.Load() {
		return false
	}
	snap := r.snapshot.Load().(*snapshot)
	if snap == nil {
		return false
	}
	return r.servePublicSnapshot(w, req, ctx, sourceIP, enforceRateLimit, snap)
}

func (r *Runtime) servePublicSnapshot(w http.ResponseWriter, req *http.Request, ctx publicsurface.Context, sourceIP string, enforceRateLimit bool, snap *snapshot) bool {
	requestPath := req.URL.Path
	if requestPath == "" {
		requestPath = "/"
	}
	path, err := fallbackdomain.NormalizePublicPath(requestPath)
	if err != nil || fallbackdomain.IsReservedPublicPath(path, []string{ctx.AdminBasePath}) {
		return false
	}
	if requestPath != path {
		if !r.allowPublicRequestIfNeeded(w, sourceIP, snap.csp, enforceRateLimit) {
			return true
		}
		if redirect, ok := snap.redirects[path]; ok {
			r.redirect(w, req, redirect.toPath, redirect.statusCode, snap.csp)
			return true
		}
		if _, ok := snap.pages[path]; ok {
			r.redirect(w, req, path, http.StatusPermanentRedirect, snap.csp)
			return true
		}
	}
	file, ok := snap.pages[path]
	if !r.allowPublicRequestIfNeeded(w, sourceIP, snap.csp, enforceRateLimit) {
		return true
	}
	if !ok {
		if redirect, redirectOK := snap.redirects[path]; redirectOK {
			r.redirect(w, req, redirect.toPath, redirect.statusCode, snap.csp)
			return true
		}
		if !snap.hasNotFound {
			return false
		}
		r.writeHTTPFile(w, req, snap.notFound, http.StatusNotFound, snap.csp)
		return true
	}
	if req.Method == http.MethodPost && path == "/" {
		r.writeHTTPFile(w, req, file, http.StatusOK, snap.csp)
		return true
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	r.writeHTTPFile(w, req, file, http.StatusOK, snap.csp)
	return true
}

func (r *Runtime) allowPublicRequestIfNeeded(w http.ResponseWriter, sourceIP, csp string, enforce bool) bool {
	if !enforce || r.limiter == nil {
		return true
	}
	key := sourceIP
	if key == "" {
		key = "unknown"
	}
	decision := r.limiter.Allow(key)
	if decision.Allowed {
		return true
	}
	setPublicHTTPHeaders(w, csp)
	retryAfter := int((decision.RetryAfter + time.Second - 1) / time.Second)
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.WriteHeader(http.StatusTooManyRequests)
	return false
}
func (r *Runtime) redirect(w http.ResponseWriter, req *http.Request, target string, status int, csp string) {
	setPublicHTTPHeaders(w, csp)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, req, target, status)
}

func requestCacheFresh(req *http.Request, etag string, modified int64) bool {
	if req == nil {
		return false
	}
	if match := strings.TrimSpace(req.Header.Get("If-None-Match")); match != "" {
		for _, candidate := range strings.Split(match, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || candidate == etag {
				return true
			}
		}
	}
	if modified <= 0 {
		return false
	}
	value := strings.TrimSpace(req.Header.Get("If-Modified-Since"))
	if value == "" {
		return false
	}
	since, err := http.ParseTime(value)
	if err != nil {
		return false
	}
	return !time.Unix(modified, 0).UTC().After(since.UTC())
}

func publicSiteCSP(resources []fallbackdomain.ExternalResource) string {
	imgSrc := []string{"'self'", "data:"}
	fontSrc := []string{"'self'", "data:"}
	for _, resource := range resources {
		origin := externalOrigin(resource.URL)
		if origin == "" {
			continue
		}
		switch resource.Kind {
		case "image":
			imgSrc = appendUnique(imgSrc, origin)
		case "font":
			fontSrc = appendUnique(fontSrc, origin)
		}
	}
	return "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; img-src " +
		strings.Join(imgSrc, " ") +
		"; font-src " + strings.Join(fontSrc, " ") +
		"; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'none'; frame-src 'none'; form-action 'none'"
}

func externalOrigin(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

// Shutdown stops accepting feature requests and drains both standalone and
// shared Gin work. Failed drain retains snapshots, handles and registrations
// for observation and a later retry; durable publication authority is untouched.
func (r *Runtime) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, runtimeDrainTimeout)
	defer cancel()
	if err := r.lockContext(ctx); err != nil {
		return err
	}
	defer r.mu.Unlock()
	r.workMu.Lock()
	r.draining.Store(true)
	idle := r.idle
	r.workMu.Unlock()
	if err := r.serving.Drain(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-idle:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.unregister != nil {
		r.unregister()
		r.unregister = nil
	}
	r.snapshot.Store((*snapshot)(nil))
	r.source = nil
	r.stopped = true
	if r.limiter != nil {
		r.limiter.ResetAll()
	}
	return nil
}

type observationResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *observationResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *observationResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	written, err := w.ResponseWriter.Write(data)
	w.bytes += int64(written)
	return written, err
}

func (w *observationResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (r *Runtime) emitHTTPObservation(req *http.Request, status int, bytes int64, siteID uint, duration time.Duration) {
	if req == nil {
		return
	}
	sourceIP := req.RemoteAddr
	if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		sourceIP = host
	}
	r.emitPublicObservation(req, sourceIP, status, bytes, siteID, duration)
}

func (r *Runtime) emitPublicObservation(req *http.Request, sourceIP string, status int, bytes int64, siteID uint, duration time.Duration) {
	if !r.surface.ObservationsEnabled() {
		return
	}
	requestURI := req.URL.RequestURI()
	r.surface.Observe(publicsurface.Observation{
		ResourceID:     "component:fallback-html:site:" + strconv.FormatUint(uint64(siteID), 10),
		ResourceKind:   "public_site",
		ComponentID:    "fallback-html",
		SourceIP:       strings.TrimSpace(sourceIP),
		MethodClass:    publicsurface.ClassifyMethod(req.Method),
		PathClass:      publicsurface.ClassifyPath(requestURI, false),
		StatusClass:    publicsurface.ClassifyStatus(status),
		UserAgentClass: publicsurface.ClassifyUserAgent(req.UserAgent()),
		BytesClass:     publicsurface.ClassifyBytes(bytes),
		DurationClass:  publicsurface.ClassifyDuration(duration.Milliseconds()),
		RateLimited:    status == http.StatusTooManyRequests,
		ObservedAt:     time.Now().Unix(),
	})
}
