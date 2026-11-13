//go:build !minimal

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
	"github.com/MalenkiySolovey/solovey-host/componenthost/publicsurface"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testSurface struct {
	mu         sync.Mutex
	registered bool
	events     []publicsurface.Observation
	handler    PublicHandler
}

func (s *testSurface) RegisterPublic(h PublicHandler) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registered {
		return nil, errors.New("already registered")
	}
	s.registered = true
	s.handler = h
	return func() { s.mu.Lock(); defer s.mu.Unlock(); s.registered = false; s.handler = nil }, nil
}
func (s *testSurface) ObservationsEnabled() bool { return true }
func (s *testSurface) Observe(o publicsurface.Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, o)
}

type testServing struct {
	mu            sync.Mutex
	ready         bool
	fail          bool
	binds, drains int
}

func (s *testServing) Owns(string, int) bool                         { s.mu.Lock(); defer s.mu.Unlock(); return s.ready }
func (s *testServing) OwnsKey(string) bool                           { return s.Owns("", 0) }
func (s *testServing) TLSAcceptsServerName(string, int, string) bool { return false }
func (s *testServing) Drain(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drains++
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.fail {
		return errors.New("drain incomplete")
	}
	s.ready = false
	return nil
}

type testBinding struct {
	scope HTTPBindingScope
	bind  func(context.Context, HTTPBindingScope) error
}

func (b *testBinding) Scope() HTTPBindingScope                            { return b.scope }
func (b *testBinding) Bind(ctx context.Context, s HTTPBindingScope) error { return b.bind(ctx, s) }

type testSource struct {
	runtime     *Runtime
	serving     *testServing
	candidate   *PublicationCandidate
	fail        error
	tamperScope bool
}

func (s *testSource) LoadPublication(ctx context.Context, generation uint64) (*PublicationCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.fail != nil {
		return nil, s.fail
	}
	if s.candidate == nil {
		return nil, nil
	}
	c := *s.candidate
	scope := HTTPBindingScope{Resource: ConfiguredTargetIdentity(c.Publish.SiteID, fallbackdomain.RuntimeTarget{}), PublicationRevision: c.Publish.Version, ExpectedGeneration: generation}
	if len(c.Targets) == 1 {
		scope.Resource = ConfiguredTargetIdentity(c.Publish.SiteID, c.Targets[0])
	}
	if s.tamperScope {
		scope.PublicationRevision = "foreign"
	}
	consumed := false
	c.Binding = &testBinding{scope: scope, bind: func(ctx context.Context, actual HTTPBindingScope) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if actual != scope || !s.runtime.BindingCurrent(actual) || consumed {
			return errors.New("stale binding")
		}
		consumed = true
		s.serving.mu.Lock()
		defer s.serving.mu.Unlock()
		s.serving.binds++
		s.serving.ready = true
		return nil
	}}
	return &c, nil
}
func testCandidate() *PublicationCandidate {
	home := []byte("<h1>Published home</h1>")
	missing := []byte("<h1>Missing page</h1>")
	file := func(p string, d []byte) fallbackdomain.PublishFile {
		sum := sha256.Sum256(d)
		return fallbackdomain.PublishFile{PublicPath: p, FilePath: "owned/" + p, MimeType: "text/html; charset=utf-8", Sha256: hex.EncodeToString(sum[:]), CachePolicy: htmlCachePolicy, SizeBytes: int64(len(d))}
	}
	return &PublicationCandidate{Publish: fallbackdomain.Publish{SiteID: 7, Version: "revision-1", CreatedAt: 1700000000, Files: []fallbackdomain.PublishFile{file("/", home), file("/404.html", missing)}, Redirects: []fallbackdomain.PublishRedirect{{FromPath: "/old/", ToPath: "/", StatusCode: 302}}}, Files: [][]byte{home, missing}}
}
func testRuntime(t *testing.T) (*Runtime, *testSource, *testSurface, *testServing) {
	t.Helper()
	surface := &testSurface{}
	serving := &testServing{}
	r := NewRuntime(serving, surface, nil)
	source := &testSource{runtime: r, serving: serving, candidate: testCandidate()}
	t.Cleanup(func() {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return r, source, surface, serving
}
func serveShared(r *Runtime, method, url string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, url, nil)
	r.ServePublic(w, req, publicsurface.Context{AdminBasePath: "/private-admin/"}, "198.51.100.4")
	return w
}

func TestRuntimeVerifiesImmutablePublicationAndScopedBinding(t *testing.T) {
	r, source, _, serving := testRuntime(t)
	if err := r.Start(source); err != nil {
		t.Fatal(err)
	}
	before := r.PublicationState()
	if before.Generation != 1 || before.PublicationRevision != "revision-1" {
		t.Fatalf("state=%#v", before)
	}
	source.candidate.Files[0][0] = 'X'
	if got := serveShared(r, "GET", "/").Body.String(); got != "<h1>Published home</h1>" {
		t.Fatal(got)
	}
	if err := r.Rebuild(source); err == nil {
		t.Fatal("changed bytes activated")
	}
	if got := r.PublicationState(); got != before {
		t.Fatalf("failed integrity changed snapshot: %#v", got)
	}
	source.candidate = testCandidate()
	source.tamperScope = true
	if err := r.Rebuild(source); err == nil {
		t.Fatal("foreign binding activated")
	}
	if serving.binds != 1 {
		t.Fatalf("bind attempts=%d", serving.binds)
	}
	source.tamperScope = false
	source.candidate.Files = source.candidate.Files[:1]
	if err := r.Rebuild(source); err == nil {
		t.Fatal("incomplete publication activated")
	}
}

func TestRuntimeHTTPBytesCacheRedirectsAndReservedRoutes(t *testing.T) {
	r, source, surface, _ := testRuntime(t)
	if err := r.Start(source); err != nil {
		t.Fatal(err)
	}
	for _, standalone := range []bool{false, true} {
		for _, tc := range []struct {
			method, path   string
			status         int
			body, location string
		}{{"GET", "/", 200, "<h1>Published home</h1>", ""}, {"POST", "/", 200, "<h1>Published home</h1>", ""}, {"HEAD", "/", 200, "", ""}, {"DELETE", "/", 405, "", ""}, {"GET", "/missing/", 404, "<h1>Missing page</h1>", ""}, {"GET", "/old/", 302, "", "/"}} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if standalone {
				r.ServeHTTP(w, req)
			} else if !r.ServePublic(w, req, publicsurface.Context{AdminBasePath: "/private-admin/"}, "198.51.100.4") {
				t.Fatal("route not handled")
			}
			if w.Code != tc.status || w.Header().Get("Location") != tc.location {
				t.Fatalf("standalone=%v %s %s: %d %#v", standalone, tc.method, tc.path, w.Code, w.Header())
			}
			if tc.location == "" && w.Body.String() != tc.body {
				t.Fatalf("body=%q", w.Body.String())
			}
		}
	}
	for _, p := range []string{"/private-admin/", "/.well-known/acme-challenge/token", "/api/users"} {
		w := httptest.NewRecorder()
		if r.ServePublic(w, httptest.NewRequest("GET", p, nil), publicsurface.Context{AdminBasePath: "/private-admin/"}, "safe") {
			t.Fatalf("reserved path handled %s", p)
		}
	}
	first := serveShared(r, "GET", "/")
	for _, header := range []string{"ETag", "Last-Modified"} {
		req := httptest.NewRequest("GET", "/", nil)
		if header == "ETag" {
			req.Header.Set("If-None-Match", first.Header().Get(header))
		} else {
			req.Header.Set("If-Modified-Since", first.Header().Get(header))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 304 || w.Body.Len() != 0 {
			t.Fatalf("cache %s: %d %s", header, w.Code, w.Body)
		}
	}
	count := len(surface.events)
	if report := r.Health(publicsurface.Context{AdminBasePath: "/private-admin/"}); !report.OK {
		t.Fatalf("health=%#v", report)
	}
	if len(surface.events) != count {
		t.Fatal("health emitted request observation")
	}
	for _, e := range surface.events {
		if strings.Contains(e.PathClass, "private-admin") || strings.Contains(e.SourceIP, "secret") {
			t.Fatalf("observation leak %#v", e)
		}
	}
}

type blockResponse struct {
	http.ResponseWriter
	entered, release chan struct{}
}

func (w *blockResponse) Write(b []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseWriter.Write(b)
}
func TestShutdownRetainsInflightSnapshotAndRegistrationForRetry(t *testing.T) {
	r, source, surface, _ := testRuntime(t)
	if err := r.Start(source); err != nil {
		t.Fatal(err)
	}
	w := &blockResponse{ResponseWriter: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServePublic(w, httptest.NewRequest("GET", "/", nil), publicsurface.Context{AdminBasePath: "/private/"}, "safe")
	}()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := r.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !r.Status().Active || !r.PublicationState().Draining || !surface.registered {
		t.Fatalf("failed Stop discarded state: %v %#v", err, r.PublicationState())
	}
	if err := r.Start(source); !errors.Is(err, ErrRuntimeDraining) {
		t.Fatalf("Start during drain: %v", err)
	}
	if err := r.Rebuild(source); !errors.Is(err, ErrRuntimeDraining) {
		t.Fatalf("Rebuild during drain: %v", err)
	}
	close(w.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not finish")
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Status().Active || surface.registered {
		t.Fatal("successful retry retained live state")
	}
	if err := r.Start(source); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownCancellationDuringRuntimeContentionDoesNotChangeAdmission(t *testing.T) {
	r, _, _, _ := testRuntime(t)
	r.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := r.Shutdown(ctx)
	r.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) || r.draining.Load() {
		t.Fatalf("contention=%v draining=%v", err, r.draining.Load())
	}
}
func TestServingDrainFailureRetainsSnapshotAndSupportsRetry(t *testing.T) {
	r, source, surface, serving := testRuntime(t)
	if err := r.Start(source); err != nil {
		t.Fatal(err)
	}
	serving.fail = true
	if err := r.Shutdown(context.Background()); err == nil {
		t.Fatal("drain failure accepted")
	}
	if !surface.registered || !r.Status().Active {
		t.Fatal("failed drain lost handles")
	}
	serving.fail = false
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestStartWithoutPublicationHasNoBindAndRejectsDuplicateSurface(t *testing.T) {
	r, source, surface, serving := testRuntime(t)
	source.candidate = nil
	if err := r.Start(source); err != nil {
		t.Fatal(err)
	}
	if serving.binds != 0 || r.Status().Active {
		t.Fatal("empty source activated")
	}
	other := NewRuntime(&testServing{}, surface, nil)
	if err := other.Start(source); err == nil {
		t.Fatal("duplicate public surface registration accepted")
	}
}
