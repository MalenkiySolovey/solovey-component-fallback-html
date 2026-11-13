//go:build !minimal

package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestLegacySelfStealDecoderIsBoundedStrictAndNonActionable(t *testing.T) {
	raw := legacySelfStealPayload()
	inspection, err := DecodeLegacySelfStealPayload(raw)
	if err != nil || inspection.Classification != "RETIRED_NON_ACTIONABLE" ||
		!containsString(inspection.ReasonCodes, LegacySelfStealRetiredReason) {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	encoded, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-key", "/private/path", "203.0.113.10", "443", "tlsRecordId"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("inspection leaked %q: %s", forbidden, encoded)
		}
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"schema":"solovey-ui/fallback-html-self-steal-draft/v1","source":"fallback-html:self-steal","unknown":true}`),
		json.RawMessage(`{"schema":"solovey-ui/fallback-html-self-steal-draft/v1","source":"other"}`),
		json.RawMessage(`{`),
	} {
		inspection, err = DecodeLegacySelfStealPayload(invalid)
		if err == nil || inspection.Classification != "LEGACY_INVALID_NON_ACTIONABLE" {
			t.Fatalf("invalid payload became actionable: %#v err=%v", inspection, err)
		}
	}
	oversized := make(json.RawMessage, maxLegacySelfStealPayloadBytes+1)
	if _, err := DecodeLegacySelfStealPayload(oversized); err == nil {
		t.Fatal("oversized legacy payload was accepted")
	}
}

func legacySelfStealPayload() json.RawMessage {
	return json.RawMessage(`{"schema":"solovey-ui/fallback-html-self-steal-draft/v1","source":"fallback-html:self-steal","profile":"vless-reality","noApply":true,"requiresCapability":"inbound-draft","target":{"port":443},"handshakeTarget":{"path":"/private/path"},"publicListen":"203.0.113.10","publicPort":443,"tlsRecordId":9,"inboundCandidate":{"private_key":"secret-key"},"warnings":[],"blocks":[],"conservativeDefaults":[],"nextSteps":[]}`)
}

func TestRemoteTemplateURLStaysOnCatalogHost(t *testing.T) {
	if _, err := resolveRemoteTemplateURL("https://raw.githubusercontent.com/MalenkiySolovey/solovey-fallback-pages/main/templates/catalog.json", "https://example.com/template/manifest.json"); err == nil {
		t.Fatalf("absolute cross-host remote template URL should be rejected")
	}
	resolved, err := resolveRemoteTemplateURL("https://raw.githubusercontent.com/MalenkiySolovey/solovey-fallback-pages/main/templates/catalog.json", "webmail-workspace/manifest.json")
	if err != nil {
		t.Fatalf("same-host relative template URL: %v", err)
	}
	if !strings.Contains(resolved, "/templates/webmail-workspace/manifest.json") {
		t.Fatalf("unexpected resolved URL: %s", resolved)
	}
}

func TestRemoteTemplateMetadataIsStrictAndBounded(t *testing.T) {
	var catalog remoteCatalogFile
	if err := decodeRemoteJSON([]byte(`{"schema":"solovey-ui/fallback-decoy-catalog/v1","templates":[],"unexpected":true}`), &catalog); err == nil {
		t.Fatal("unknown remote catalog fields should be rejected")
	}
	if err := decodeRemoteJSON([]byte(`{"schema":"solovey-ui/fallback-decoy-catalog/v1","templates":[]} {}`), &catalog); err == nil {
		t.Fatal("multiple remote catalog documents should be rejected")
	}
	manifest := remoteTemplateManifest{
		Schema: "solovey-ui/fallback-decoy-template/v1",
		ID:     "unsafe/id",
		Name:   "Unsafe",
		Pages:  []string{"pages/index.html"},
		Assets: []string{"assets/decoy-interactivity.js"},
	}
	if err := validateRemoteManifest(manifest.ID, manifest); err == nil {
		t.Fatal("unsafe remote template ids should be rejected")
	}
	manifest.ID = "bounded-template"
	manifest.Pages = make([]string, maxRemoteTemplateFiles)
	for index := range manifest.Pages {
		manifest.Pages[index] = fmt.Sprintf("pages/page-%d.html", index)
	}
	if err := validateRemoteManifest(manifest.ID, manifest); err == nil {
		t.Fatal("remote templates with too many files should be rejected")
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
