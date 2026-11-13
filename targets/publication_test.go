//go:build !minimal

package targets

import (
	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
	"github.com/MalenkiySolovey/solovey-host/componenthost/resources"
	"testing"
)

func TestPublishedIdentityIsStableAcrossRowOrderAndIncludesRedirectPolicy(t *testing.T) {
	p := fallbackdomain.Publish{Version: "r1", Files: []fallbackdomain.PublishFile{{PublicPath: "/b/", Sha256: "b", SizeBytes: 2}, {PublicPath: "/", Sha256: "a", SizeBytes: 1}}, Redirects: []fallbackdomain.PublishRedirect{{FromPath: "/old/", ToPath: "/", StatusCode: 302}}}
	original := PublishDigestV2(p)
	p.Files[0], p.Files[1] = p.Files[1], p.Files[0]
	if PublishDigestV2(p) != original {
		t.Fatal("file row order changed identity")
	}
	p.Redirects[0].StatusCode = 301
	if PublishDigestV2(p) == original {
		t.Fatal("redirect policy change was not fenced")
	}
	p.Redirects[0].ToPath = "https://example.com/"
	p.Redirects[0].External = true
	if _, reachable, reason := PublishedRedirectFacts(p); reachable != resources.CapabilityUnknown || reason != "external_redirect_unverified" {
		t.Fatalf("external route advertised safe: %s %s", reachable, reason)
	}
}
func TestRedirectSafetyRejectsNoncanonicalAndReservedTargets(t *testing.T) {
	for _, r := range []fallbackdomain.PublishRedirect{{FromPath: "/old", ToPath: "/", StatusCode: 302}, {FromPath: "/old/", ToPath: "/api/users/", StatusCode: 302}, {FromPath: "/old/", ToPath: "/", StatusCode: 200}, {FromPath: "/old/", ToPath: "/old/", StatusCode: 302}, {FromPath: "/old/", ToPath: "https://127.0.0.1/", StatusCode: 302, External: true}} {
		if _, reason := ClassifyPublishedRedirect(r); reason != "redirect_target_invalid" {
			t.Fatalf("invalid redirect accepted: %#v %s", r, reason)
		}
	}
	if class, reason := ClassifyPublishedRedirect(fallbackdomain.PublishRedirect{FromPath: "/old/", ToPath: "/", StatusCode: 302}); class != "provider_local_relative" || reason != "" {
		t.Fatalf("safe redirect: %s %s", class, reason)
	}
}
func TestCanonicalServerNamesExcludeIPsAndInvalidLabels(t *testing.T) {
	if CanonicalServerName("Decoy.Example.") != "decoy.example" {
		t.Fatal("DNS normalization changed")
	}
	for _, s := range []string{"127.0.0.1", "::1", "bad..example", "-bad.example", "bad-.example", "a/b.example", ""} {
		if CanonicalServerName(s) != "" {
			t.Fatalf("unsafe server name %q", s)
		}
	}
}
