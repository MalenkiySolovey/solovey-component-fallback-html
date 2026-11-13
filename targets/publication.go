//go:build !minimal

// Package targets contains publication and redirect identity algorithms.
package targets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
	hostresources "github.com/MalenkiySolovey/solovey-host/componenthost/resources"
	"net"
	"sort"
	"strings"
)

const PublishedRedirectPolicyRevision = "fallback-html-published-redirect-safety-v1"

type PublishedRedirectFact struct {
	FromDigest string `json:"fromDigest"`
	ToDigest   string `json:"toDigest"`
	StatusCode int    `json:"statusCode"`
	Class      string `json:"class"`
}

func PublishDigest(publish fallbackdomain.Publish) string {
	type fileFact struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	files := make([]fileFact, 0, len(publish.Files))
	for _, file := range publish.Files {
		files = append(files, fileFact{file.PublicPath, file.Sha256, file.SizeBytes})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	payload, _ := json.Marshal(struct {
		Version string     `json:"version"`
		Files   []fileFact `json:"files"`
	}{publish.Version, files})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func PublishDigestV2(publish fallbackdomain.Publish) string {
	redirects, _, _ := PublishedRedirectFacts(publish)
	payload, _ := json.Marshal(struct {
		PublishDigest  string                  `json:"PublishDigest"`
		RedirectPolicy string                  `json:"redirectPolicy"`
		Redirects      []PublishedRedirectFact `json:"redirects"`
	}{PublishDigest(publish), PublishedRedirectPolicyRevision, redirects})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func PublishedRedirectFacts(publish fallbackdomain.Publish) ([]PublishedRedirectFact, hostresources.CapabilityValue, string) {
	facts := make([]PublishedRedirectFact, 0, len(publish.Redirects))
	reason := ""
	for _, redirect := range publish.Redirects {
		class, currentReason := ClassifyPublishedRedirect(redirect)
		facts = append(facts, PublishedRedirectFact{
			FromDigest: digestPublishedRedirectValue(redirect.FromPath),
			ToDigest:   digestPublishedRedirectValue(redirect.ToPath),
			StatusCode: redirect.StatusCode,
			Class:      class,
		})
		if currentReason == "redirect_target_invalid" || reason == "" {
			reason = currentReason
		}
	}
	sort.Slice(facts, func(i, j int) bool {
		if facts[i].FromDigest != facts[j].FromDigest {
			return facts[i].FromDigest < facts[j].FromDigest
		}
		if facts[i].ToDigest != facts[j].ToDigest {
			return facts[i].ToDigest < facts[j].ToDigest
		}
		if facts[i].StatusCode != facts[j].StatusCode {
			return facts[i].StatusCode < facts[j].StatusCode
		}
		return facts[i].Class < facts[j].Class
	})
	if reason != "" {
		return facts, hostresources.CapabilityUnknown, reason
	}
	return facts, hostresources.CapabilityNo, ""
}

func ClassifyPublishedRedirect(redirect fallbackdomain.PublishRedirect) (string, string) {
	fromPath, err := fallbackdomain.ValidatePagePath(redirect.FromPath, nil)
	if err != nil || fromPath != redirect.FromPath {
		return "unknown", "redirect_target_invalid"
	}
	statusCode, err := fallbackdomain.ValidateRedirectStatus(redirect.StatusCode)
	if err != nil || statusCode != redirect.StatusCode {
		return "unknown", "redirect_target_invalid"
	}
	toPath, external, err := fallbackdomain.NormalizeRedirectTarget(redirect.ToPath, nil)
	if err != nil || toPath != redirect.ToPath || external != redirect.External {
		return "unknown", "redirect_target_invalid"
	}
	if external {
		return "external_absolute", "external_redirect_unverified"
	}
	if fromPath == toPath {
		return "unknown", "redirect_target_invalid"
	}
	return "provider_local_relative", ""
}

func digestPublishedRedirectValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func CanonicalServerName(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(value, ".")))
	if value == "" || len(value) > 253 || net.ParseIP(value) != nil {
		return ""
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return ""
		}
		for _, character := range label {
			if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
				continue
			}
			return ""
		}
	}
	return value
}
