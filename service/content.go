//go:build !minimal

package service

import fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"

// Content binds only Fallback-owned site assets, immutable publications and
// reconstructable template packages. The Backend validates every stored path
// against its owning site and retains all filesystem and rename authority.
type Content interface {
	ReadAsset(fallbackdomain.Asset) ([]byte, error)
	CreateAsset(siteID uint, filename string, data []byte) (string, error)
	RemoveCreatedAsset(siteID uint, storedPath string) error
	StageAssetRemoval(fallbackdomain.Asset) (ContentChange, error)
	BeginPublication(siteID uint, version string) (PublicationDraft, error)
	ReadPublishFile(fallbackdomain.Publish, fallbackdomain.PublishFile) ([]byte, error)
	ReadPublishMetadata(fallbackdomain.Publish, ArtifactPart) ([]byte, error)
	RemovePublication(siteID uint, root string) error
	RemoveSite(siteID uint) error
	ReadTemplate(templateID, relative string) ([]byte, error)
	ReplaceTemplate(templateID string, files []TemplateFile) (ContentChange, error)
	RemoveTemplate(templateID string) (ContentChange, error)
	TemplateAvailable(templateID string) bool
}

// ContentChange retains the exact staged file or directory for transaction
// compensation. Commit discards the staged previous content; Rollback restores it.
type ContentChange interface {
	Commit() error
	Rollback() error
}

// PublicationDraft is issued for one site and revision. Public files use the
// canonical public-path mapping; metadata accepts only the declared artifact parts.
type PublicationDraft interface {
	RootDir() string
	WritePublicFile(publicPath string, data []byte) (string, error)
	WriteArtifact(ArtifactPart, []byte) error
	Discard() error
}
type ArtifactPart string

const (
	ArtifactSite      ArtifactPart = "site.json"
	ArtifactTargets   ArtifactPart = "targets.json"
	ArtifactRoutes    ArtifactPart = "routes.json"
	ArtifactSafety    ArtifactPart = "safety-report.json"
	ArtifactNode      ArtifactPart = "node-artifact.json"
	ArtifactChecksums ArtifactPart = "checksums.txt"
)

func (p ArtifactPart) Valid() bool {
	switch p {
	case ArtifactSite, ArtifactTargets, ArtifactRoutes, ArtifactSafety, ArtifactNode, ArtifactChecksums:
		return true
	}
	return false
}

type TemplateFile struct {
	Relative string
	Data     []byte
}
