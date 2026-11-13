package fallbackhtml

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestManifestRetainsComponentAndDurableIdentity(t *testing.T) {
	data := ManifestJSON()
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != "5593b4c3ad2c256b7ccd2539123e06f2817a12df39b49fbfbb0ea4e7077fcb9c" {
		t.Fatal("manifest identity changed")
	}
	var m struct {
		ID, Delivery   string
		DefaultEnabled bool
		Database       struct {
			SchemaVersion, MigrationVersion string
			Tables                          []string
		}
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "fallback-html" || m.Delivery != "in-process" || m.DefaultEnabled || m.Database.SchemaVersion != "1" || m.Database.MigrationVersion != "1" || len(m.Database.Tables) != 15 {
		t.Fatalf("manifest=%#v", m)
	}
	data[0] = 'x'
	if ManifestJSON()[0] == 'x' {
		t.Fatal("descriptor shares mutable bytes")
	}
}
