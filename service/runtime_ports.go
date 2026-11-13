//go:build !minimal

package service

import (
	"context"
	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
	"github.com/MalenkiySolovey/solovey-host/componenthost/publicsurface"
	"net/http"
	"time"
)

// PublicationSource reads the already admitted publication from its exact
// Backend storage generation. Candidate storage never becomes the cached live
// source. The binding retains the Backend's socket and certificate authority.
type PublicationSource interface {
	LoadPublication(context.Context, uint64) (*PublicationCandidate, error)
}
type PublicationCandidate struct {
	Publish   fallbackdomain.Publish
	Resources []fallbackdomain.ExternalResource
	Targets   []fallbackdomain.RuntimeTarget
	Files     [][]byte
	Binding   PublicationBinding
}
type PublicHandler func(http.ResponseWriter, *http.Request, publicsurface.Context, string) bool
type PublicSurface interface {
	RegisterPublic(PublicHandler) (func(), error)
	ObservationsEnabled() bool
	Observe(publicsurface.Observation)
}
type RequestLimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}
type RequestLimiter interface {
	Allow(string) RequestLimitDecision
	ResetAll()
}

func NewRuntime(serving HTTPServing, surface PublicSurface, limiter RequestLimiter) *Runtime {
	if serving == nil || surface == nil {
		panic("fallback-html serving and public surface owners are required")
	}
	r := &Runtime{serving: serving, surface: surface, limiter: limiter, idle: make(chan struct{})}
	close(r.idle)
	r.snapshot.Store((*snapshot)(nil))
	return r
}
func (r *Runtime) BindingCurrent(scope HTTPBindingScope) bool {
	return scope.ExpectedGeneration == r.generation.Load() && !r.draining.Load()
}
func (r *Runtime) TLSAcceptsServerName(listen string, port int, serverName string) bool {
	return !r.draining.Load() && r.serving.TLSAcceptsServerName(listen, port, serverName)
}
