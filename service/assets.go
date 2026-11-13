//go:build !minimal

package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
)

type AssetView struct {
	ID          uint   `json:"id"`
	LogicalPath string `json:"logicalPath"`
	MimeType    string `json:"mimeType"`
	Sha256      string `json:"sha256"`
	SizeBytes   int64  `json:"sizeBytes"`
	Provenance  string `json:"provenance"`
	CreatedAt   int64  `json:"createdAt"`
}

type ExternalResourceView struct {
	ID        uint   `json:"id"`
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	Allowed   bool   `json:"allowed"`
	CreatedAt int64  `json:"createdAt"`
}

const maxAssetBytes = 1024 * 1024

var maxSiteAssetBytes int64 = 16 * maxAssetBytes

const (
	htmlCachePolicy    = "public, max-age=60, must-revalidate"
	assetCachePolicy   = "public, max-age=31536000, immutable"
	serviceCachePolicy = "public, max-age=3600, must-revalidate"
	noStoreCachePolicy = "no-store"
)

var assetMimeByExt = map[string]string{
	".css":   "text/css; charset=utf-8",
	".ico":   "image/x-icon",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".png":   "image/png",
	".txt":   "text/plain; charset=utf-8",
	".webp":  "image/webp",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

var blockedAssetExtensions = map[string]struct{}{
	".7z":   {},
	".bat":  {},
	".cgi":  {},
	".cmd":  {},
	".conf": {},
	".dll":  {},
	".env":  {},
	".exe":  {},
	".gz":   {},
	".key":  {},
	".pem":  {},
	".php":  {},
	".rar":  {},
	".sh":   {},
	".so":   {},
	".tar":  {},
	".xz":   {},
	".zip":  {},
}

func (s *Service) ListAssets(siteID uint) ([]AssetView, error) {
	var assets []fallbackdomain.Asset
	err := s.db.ListAssets(siteID, &assets)
	if err != nil {
		return nil, err
	}
	out := make([]AssetView, 0, len(assets))
	for _, asset := range assets {
		out = append(out, assetView(asset))
	}
	return out, nil
}

func (s *Service) SaveAsset(siteID uint, filename string, data []byte, actor string) (AssetView, error) {
	return s.saveAsset(siteID, filename, data, actor, "uploaded")
}

func (s *Service) saveAsset(siteID uint, filename string, data []byte, actor string, provenance string) (AssetView, error) {
	if len(data) == 0 {
		return AssetView{}, errors.New("asset file is empty")
	}
	if len(data) > maxAssetBytes {
		return AssetView{}, fmt.Errorf("asset file is larger than %d bytes", maxAssetBytes)
	}
	safeName, mimeType, err := validateAssetFile(filename, data)
	if err != nil {
		return AssetView{}, err
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	logicalPath, err := fallbackdomain.ValidatePagePath("/media/"+sha[:12]+"-"+safeName, nil)
	if err != nil {
		return AssetView{}, err
	}
	now := time.Now().Unix()
	var asset fallbackdomain.Asset
	createdPath := ""
	err = s.db.Transaction(func(tx Store) error {
		if err := tx.LoadSite(siteID, &fallbackdomain.Site{}); err != nil {
			return err
		}
		var existing fallbackdomain.Asset
		err := tx.LoadAssetPath(siteID, logicalPath, &existing)
		if err == nil {
			data, err := s.content.ReadAsset(existing)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != existing.Sha256 {
				return fmt.Errorf("fallback-html asset %s failed integrity check", existing.LogicalPath)
			}
			asset = existing
			return nil
		}
		if !errors.Is(err, ErrRecordNotFound) {
			return err
		}
		if err := ensureSiteAssetQuota(tx, siteID, int64(len(data))); err != nil {
			return err
		}
		diskPath, err := s.content.CreateAsset(siteID, sha[:12]+"-"+safeName, data)
		if err != nil {
			return err
		}
		createdPath = diskPath
		asset = fallbackdomain.Asset{
			SiteID:      siteID,
			LogicalPath: logicalPath,
			FilePath:    diskPath,
			MimeType:    mimeType,
			Sha256:      sha,
			SizeBytes:   int64(len(data)),
			Provenance:  provenance,
			CreatedAt:   now,
		}
		if err := tx.CreateAsset(&asset); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "asset_uploaded", map[string]any{"path": logicalPath, "size": len(data)})
	})
	if err != nil {
		if createdPath != "" {
			_ = s.content.RemoveCreatedAsset(siteID, createdPath)
		}
		return AssetView{}, err
	}
	return assetView(asset), nil
}

func (s *Service) DeleteAsset(siteID, assetID uint, actor string) error {
	var asset fallbackdomain.Asset
	var staged ContentChange
	err := s.db.Transaction(func(tx Store) error {
		if err := tx.LoadAsset(siteID, assetID, &asset); err != nil {
			return err
		}
		var err error
		staged, err = s.content.StageAssetRemoval(asset)
		if err != nil {
			return err
		}
		if err := tx.DeleteAsset(&asset); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "asset_deleted", map[string]any{"assetId": assetID})
	})
	if err != nil {
		if staged != nil {
			if restoreErr := staged.Rollback(); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore asset after rollback: %w", restoreErr))
			}
		}
		return err
	}
	if staged != nil {
		return staged.Commit()
	}
	return nil
}

func (s *Service) ListExternalResources(siteID uint) ([]ExternalResourceView, error) {
	var resources []fallbackdomain.ExternalResource
	err := s.db.ListExternalResources(siteID, &resources)
	if err != nil {
		return nil, err
	}
	out := make([]ExternalResourceView, 0, len(resources))
	for _, resource := range resources {
		out = append(out, externalResourceView(resource))
	}
	return out, nil
}

func (s *Service) SaveExternalResource(siteID uint, input ExternalResourceInput, actor string) (ExternalResourceView, error) {
	kind, err := validateExternalResourceKind(input.Kind)
	if err != nil {
		return ExternalResourceView{}, err
	}
	urlValue, external, err := fallbackdomain.NormalizeRedirectTarget(input.URL, nil)
	if err != nil {
		return ExternalResourceView{}, err
	}
	if !external {
		return ExternalResourceView{}, errors.New("external resource URL must be absolute http or https")
	}
	now := time.Now().Unix()
	var resource fallbackdomain.ExternalResource
	err = s.db.Transaction(func(tx Store) error {
		if err := tx.LoadSite(siteID, &fallbackdomain.Site{}); err != nil {
			return err
		}
		if input.ID != 0 {
			if err := tx.LoadExternalResource(siteID, input.ID, &resource); err != nil {
				return err
			}
		} else {
			resource = fallbackdomain.ExternalResource{SiteID: siteID, CreatedAt: now}
		}
		resource.Kind = kind
		resource.URL = urlValue
		resource.Allowed = input.Allowed
		if err := tx.SaveExternalResource(&resource); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "external_resource_saved", map[string]any{"kind": kind, "allowed": input.Allowed})
	})
	if err != nil {
		return ExternalResourceView{}, err
	}
	return externalResourceView(resource), nil
}

func (s *Service) DeleteExternalResource(siteID, resourceID uint, actor string) error {
	return s.db.Transaction(func(tx Store) error {
		if err := tx.DeleteExternalResource(siteID, resourceID); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "external_resource_deleted", map[string]any{"resourceId": resourceID})
	})
}

func (s *Service) loadAssets(site *fallbackdomain.Site) error {
	return s.db.ListAssets(site.ID, &site.Assets)
}

func (s *Service) externalResourcesForSite(siteID uint) ([]ExternalResourceView, error) {
	items, err := s.ListExternalResources(siteID)
	if err != nil {
		return nil, err
	}
	return items, nil
}

func assetView(asset fallbackdomain.Asset) AssetView {
	return AssetView{
		ID:          asset.ID,
		LogicalPath: asset.LogicalPath,
		MimeType:    asset.MimeType,
		Sha256:      asset.Sha256,
		SizeBytes:   asset.SizeBytes,
		Provenance:  asset.Provenance,
		CreatedAt:   asset.CreatedAt,
	}
}

func publishView(publish fallbackdomain.Publish) PublishView {
	return PublishView{
		ID:        publish.ID,
		Version:   publish.Version,
		Active:    publish.Active,
		Files:     len(publish.Files),
		CreatedAt: publish.CreatedAt,
	}
}

func validateAssetFile(filename string, data []byte) (string, string, error) {
	name := filepath.Base(strings.ReplaceAll(strings.TrimSpace(filename), "\\", "/"))
	name = strings.Trim(name, ". ")
	if name == "" {
		return "", "", errors.New("asset filename is required")
	}
	ext := strings.ToLower(filepath.Ext(name))
	if _, blocked := blockedAssetExtensions[ext]; blocked {
		return "", "", fmt.Errorf("asset extension %s is never allowed for public fallback content", ext)
	}
	mimeType, ok := assetMimeByExt[ext]
	if !ok {
		return "", "", fmt.Errorf("asset extension %s is not allowed", ext)
	}
	safe := sanitizeAssetName(name)
	if safe == "" || filepath.Ext(safe) == "" {
		return "", "", errors.New("asset filename does not contain a safe name")
	}
	if ext == ".css" {
		if !looksLikeText(data) {
			return "", "", errors.New("css asset must be valid text")
		}
		if err := fallbackdomain.ValidateStaticCSS(string(data)); err != nil {
			return "", "", errors.New("css assets must not import external resources")
		}
		return safe, mimeType, nil
	}
	if ext == ".txt" {
		if !looksLikeText(data) {
			return "", "", errors.New("text asset must be valid text")
		}
		return safe, mimeType, nil
	}
	detected := http.DetectContentType(data)
	if ext == ".woff" || ext == ".woff2" || ext == ".ico" {
		return safe, mimeType, nil
	}
	if !strings.HasPrefix(detected, strings.TrimSuffix(mimeType, "; charset=utf-8")) {
		return "", "", fmt.Errorf("asset content type %s does not match %s", detected, ext)
	}
	return safe, mimeType, nil
}

func assetLogicalPath(filename string, data []byte) (string, error) {
	safeName, _, err := validateAssetFile(filename, data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fallbackdomain.ValidatePagePath("/media/"+hex.EncodeToString(sum[:])[:12]+"-"+safeName, nil)
}

func ensureSiteAssetQuota(tx Store, siteID uint, incomingBytes int64) error {
	var used int64
	if err := tx.AssetBytesUsed(siteID, &used); err != nil {
		return err
	}
	if used+incomingBytes > maxSiteAssetBytes {
		return fmt.Errorf("fallback-html site assets would exceed %d bytes", maxSiteAssetBytes)
	}
	return nil
}

func looksLikeText(data []byte) bool {
	if len(data) == 0 || !utf8.Valid(data) {
		return false
	}
	return !bytes.Contains(data, []byte{0})
}

func externalResourceView(resource fallbackdomain.ExternalResource) ExternalResourceView {
	return ExternalResourceView{
		ID:        resource.ID,
		Kind:      resource.Kind,
		URL:       resource.URL,
		Allowed:   resource.Allowed,
		CreatedAt: resource.CreatedAt,
	}
}

func validateExternalResourceKind(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "link", "image", "font":
		return value, nil
	default:
		return "", errors.New("external resource kind must be link, image or font")
	}
}

func sanitizeAssetName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}
