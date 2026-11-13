//go:build !minimal

package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
	hostresources "github.com/MalenkiySolovey/solovey-host/componenthost/resources"
)

type PublishResult struct {
	SiteID  uint   `json:"siteId"`
	Version string `json:"version"`
	Files   int    `json:"files"`
}

type PublishView struct {
	ID        uint   `json:"id"`
	Version   string `json:"version"`
	Active    bool   `json:"active"`
	Files     int    `json:"files"`
	CreatedAt int64  `json:"createdAt"`
}

type ArtifactArchive struct {
	Filename    string
	ContentType string
	Data        []byte
	Sha256      string
	SizeBytes   int
}

type RollbackInput struct {
	Version string `json:"version"`
}

type PrunePublishesInput struct {
	Keep int `json:"keep"`
}

type PrunePublishesResult struct {
	Removed int `json:"removed"`
	Kept    int `json:"kept"`
}

type SafetyReport struct {
	OK       bool     `json:"ok"`
	Warnings []string `json:"warnings"`
}

type publishArtifact struct {
	Schema    string                    `json:"schema"`
	Version   string                    `json:"version"`
	CreatedAt int64                     `json:"createdAt"`
	Site      publishArtifactSite       `json:"site"`
	Targets   []TargetView              `json:"targets"`
	Pages     []fallbackdomain.Page     `json:"pages"`
	Redirects []fallbackdomain.Redirect `json:"redirects"`
	Assets    []AssetView               `json:"assets"`
	External  []ExternalResourceView    `json:"externalResources"`
	Files     []publishArtifactFile     `json:"files"`
	Safety    SafetyReport              `json:"safety"`
}

type publishArtifactSite struct {
	ID           uint   `json:"id"`
	Name         string `json:"name"`
	TemplateID   string `json:"templateId"`
	ExposureMode string `json:"exposureMode"`
	Hostname     string `json:"hostname"`
}

type publishArtifactFile struct {
	PublicPath  string `json:"publicPath"`
	Relative    string `json:"relative"`
	MimeType    string `json:"mimeType"`
	Sha256      string `json:"sha256"`
	SizeBytes   int64  `json:"sizeBytes"`
	CachePolicy string `json:"cachePolicy"`
}

type publishArtifactTargets struct {
	Schema    string       `json:"schema"`
	Version   string       `json:"version"`
	CreatedAt int64        `json:"createdAt"`
	Targets   []TargetView `json:"targets"`
}

type publishArtifactRoutes struct {
	Schema       string                         `json:"schema"`
	Version      string                         `json:"version"`
	CreatedAt    int64                          `json:"createdAt"`
	Pages        []publishArtifactRoutePage     `json:"pages"`
	Redirects    []publishArtifactRouteRedirect `json:"redirects"`
	CanonicalMap map[string]string              `json:"canonicalMap"`
}

type publishArtifactRoutePage struct {
	PublicPath string `json:"publicPath"`
	Relative   string `json:"relative"`
	Title      string `json:"title"`
	IsHome     bool   `json:"isHome"`
}

type publishArtifactRouteRedirect struct {
	FromPath   string `json:"fromPath"`
	ToPath     string `json:"toPath"`
	StatusCode int    `json:"statusCode"`
	External   bool   `json:"external"`
}

func (s *Service) ListPublishes(siteID uint) ([]PublishView, error) {
	var publishes []fallbackdomain.Publish
	err := s.db.ListPublishes(siteID, &publishes)
	if err != nil {
		return nil, err
	}
	out := make([]PublishView, 0, len(publishes))
	for _, publish := range publishes {
		out = append(out, publishView(publish))
	}
	return out, nil
}

func (s *Service) PrunePublishes(siteID uint, input PrunePublishesInput, actor string) (PrunePublishesResult, error) {
	keepInactive := input.Keep
	if keepInactive < 0 {
		return PrunePublishesResult{}, errors.New("keep must be zero or greater")
	}
	if keepInactive > 50 {
		keepInactive = 50
	}

	var publishes []fallbackdomain.Publish
	if err := s.db.ListPublishes(siteID, &publishes); err != nil {
		return PrunePublishesResult{}, err
	}
	if len(publishes) == 0 {
		return PrunePublishesResult{}, nil
	}

	keepIDs := make(map[uint]bool, keepInactive+1)
	inactiveKept := 0
	for _, publish := range publishes {
		if publish.Active {
			keepIDs[publish.ID] = true
			continue
		}
		if inactiveKept < keepInactive {
			keepIDs[publish.ID] = true
			inactiveKept++
		}
	}

	var removeIDs []uint
	var removeRoots []string
	for _, publish := range publishes {
		if keepIDs[publish.ID] {
			continue
		}
		removeIDs = append(removeIDs, publish.ID)
		removeRoots = append(removeRoots, publish.RootDir)
	}
	if len(removeIDs) == 0 {
		return PrunePublishesResult{Kept: len(publishes)}, nil
	}

	err := s.guardedSiteMutation(siteID, func(tx Store) error {
		if err := tx.LoadSite(siteID, &fallbackdomain.Site{}); err != nil {
			return err
		}
		if err := tx.DeletePublishRedirects(removeIDs); err != nil {
			return err
		}
		if err := tx.DeletePublishFiles(removeIDs); err != nil {
			return err
		}
		if err := tx.DeleteInactivePublishes(siteID, removeIDs); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "publishes_pruned", map[string]any{"removed": len(removeIDs), "keepInactive": keepInactive})
	}, func() error {
		for _, root := range removeRoots {
			if strings.TrimSpace(root) == "" {
				continue
			}
			if err := s.content.RemovePublication(siteID, root); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return PrunePublishesResult{}, err
	}
	return PrunePublishesResult{Removed: len(removeIDs), Kept: len(publishes) - len(removeIDs)}, nil
}

func (s *Service) GetPublishArtifact(siteID uint, version string) (ArtifactArchive, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return ArtifactArchive{}, errors.New("publish artifact version is required")
	}
	var publish fallbackdomain.Publish
	err := s.db.LoadPublish(siteID, version, &publish)
	if errors.Is(err, ErrRecordNotFound) {
		return ArtifactArchive{}, fmt.Errorf("fallback-html publish artifact %s not found", version)
	}
	if err != nil {
		return ArtifactArchive{}, err
	}
	if err := s.verifyPublishFiles(publish); err != nil {
		return ArtifactArchive{}, err
	}
	data, err := s.buildPublishArchive(publish)
	if err != nil {
		return ArtifactArchive{}, err
	}
	return ArtifactArchive{
		Filename:    "fallback-html-site-" + uintString(siteID) + "-" + SafeArchiveName(version) + ".tar.gz",
		ContentType: "application/gzip",
		Data:        data,
		Sha256:      archiveDigest(data),
		SizeBytes:   len(data),
	}, nil
}

func (s *Service) PublishSiteContext(ctx context.Context, siteID uint, actor string) (PublishResult, error) {
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	scoped := s.withStore(s.db.WithContext(ctx))
	return scoped.PublishSite(siteID, actor)
}

func (s *Service) PublishSite(siteID uint, actor string) (PublishResult, error) {
	site, err := s.GetSite(siteID)
	if err != nil {
		return PublishResult{}, err
	}
	if err := s.loadAssets(&site); err != nil {
		return PublishResult{}, err
	}
	externalResources, err := s.externalResourcesForSite(site.ID)
	if err != nil {
		return PublishResult{}, err
	}
	report, err := s.safetyForSite(site)
	if err != nil {
		return PublishResult{}, err
	}
	if blockers := blockingSafetyWarnings(report.Warnings); len(blockers) > 0 {
		return PublishResult{}, fmt.Errorf("public site is not safe to publish: %s", strings.Join(blockers, "; "))
	}
	version := strconv.FormatInt(time.Now().UnixNano(), 10)
	draft, err := s.content.BeginPublication(siteID, version)
	if err != nil {
		return PublishResult{}, err
	}
	root := draft.RootDir()
	committed := false
	defer func() {
		if !committed {
			_ = draft.Discard()
		}
	}()
	files, err := s.buildPublishFiles(draft, site)
	if err != nil {
		return PublishResult{}, err
	}
	redirects := buildPublishRedirects(site.Redirects)
	now := time.Now().Unix()
	targets, err := s.targetsForSite(site)
	if err != nil {
		return PublishResult{}, err
	}
	if err := s.ensureNoActiveGinPublishConflict(site.ID); err != nil {
		_ = draft.Discard()
		return PublishResult{}, err
	}
	if err := writePublishArtifact(draft, version, now, site, targets, externalResources, files, report); err != nil {
		_ = draft.Discard()
		return PublishResult{}, err
	}
	err = s.guardedRuntimeMutation(siteID, func(tx Store) error {
		if err := s.validatePublicationCandidate(tx, site); err != nil {
			return err
		}
		if err := ensureNoActivePublishConflict(tx, siteID); err != nil {
			return err
		}
		if err := tx.DeactivatePublishes(siteID); err != nil {
			return err
		}
		publish := fallbackdomain.Publish{SiteID: siteID, Version: version, RootDir: root, Active: true, CreatedAt: now}
		if err := tx.CreatePublish(&publish); err != nil {
			return err
		}
		for index := range files {
			files[index].PublishID = publish.ID
		}
		if err := tx.CreatePublishFiles(&files); err != nil {
			return err
		}
		for index := range redirects {
			redirects[index].PublishID = publish.ID
		}
		if len(redirects) > 0 {
			if err := tx.CreatePublishRedirects(&redirects); err != nil {
				return err
			}
		}
		if err := tx.SetSitePublication(siteID, "published", true, now); err != nil {
			return err
		}
		if err := createSafetyReport(tx, siteID, report, now); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "site_published", map[string]any{"version": version, "files": len(files)})
	})
	if err != nil {
		_ = draft.Discard()
		return PublishResult{}, err
	}
	committed = true
	return PublishResult{SiteID: siteID, Version: version, Files: len(files)}, nil
}

func (s *Service) validatePublicationCandidate(tx Store, expected fallbackdomain.Site) error {
	scoped := s.withStore(tx)
	current, err := scoped.GetSite(expected.ID)
	if err != nil {
		return err
	}
	if err := scoped.loadAssets(&current); err != nil {
		return err
	}
	if hostresources.Revision(current) != hostresources.Revision(expected) {
		return errors.New("fallback-html site or target changed while publication was prepared")
	}
	return nil
}

func blockingSafetyWarnings(warnings []string) []string {
	blockers := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		switch {
		case strings.Contains(warning, "admin web path is '/'"),
			strings.Contains(warning, "conflicts with reserved panel paths"),
			strings.Contains(warning, "targets a reserved panel path"),
			strings.Contains(warning, "site does not have a home page at /"),
			strings.Contains(warning, "publish target "):
			blockers = append(blockers, warning)
		}
	}
	return blockers
}

func (s *Service) RollbackSiteContext(ctx context.Context, siteID uint, input RollbackInput, actor string) (PublishResult, error) {
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	scoped := s.withStore(s.db.WithContext(ctx))
	return scoped.RollbackSite(siteID, input, actor)
}

func (s *Service) RollbackSite(siteID uint, input RollbackInput, actor string) (PublishResult, error) {
	site, err := s.GetSite(siteID)
	if err != nil {
		return PublishResult{}, err
	}
	if err := s.loadAssets(&site); err != nil {
		return PublishResult{}, err
	}
	if err := s.ensureNoActiveGinPublishConflict(site.ID); err != nil {
		return PublishResult{}, err
	}
	var publish fallbackdomain.Publish
	if err := s.db.LoadRollbackPublish(siteID, strings.TrimSpace(input.Version), &publish); err != nil {
		if errors.Is(err, ErrRecordNotFound) {
			return PublishResult{}, fmt.Errorf("no previous fallback-html publish is available")
		}
		return PublishResult{}, err
	}
	if err := s.verifyPublishFiles(publish); err != nil {
		return PublishResult{}, err
	}
	now := time.Now().Unix()
	err = s.guardedRuntimeMutation(siteID, func(tx Store) error {
		if err := s.validatePublicationCandidate(tx, site); err != nil {
			return err
		}
		if err := ensureNoActivePublishConflict(tx, siteID); err != nil {
			return err
		}
		if err := tx.DeactivatePublishes(siteID); err != nil {
			return err
		}
		if err := tx.ActivatePublish(publish.ID); err != nil {
			return err
		}
		if err := tx.SetSitePublication(siteID, "published", true, now); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "site_rollback", map[string]any{"version": publish.Version, "files": len(publish.Files)})
	})
	if err != nil {
		return PublishResult{}, err
	}
	return PublishResult{SiteID: siteID, Version: publish.Version, Files: len(publish.Files)}, nil
}

func (s *Service) ensureNoActiveGinPublishConflict(siteID uint) error {
	return ensureNoActivePublishConflict(s.db, siteID)
}

func ensureNoActivePublishConflict(db Store, siteID uint) error {
	var conflict fallbackdomain.Site
	err := db.PublishedSiteConflict(siteID, &conflict)
	if errors.Is(err, ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("fallback-html site %q is already published on the managed Gin listener; unpublish it before publishing another site", conflict.Name)
}

func (s *Service) UnpublishSiteContext(ctx context.Context, siteID uint, actor string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.withStore(s.db.WithContext(ctx)).UnpublishSite(siteID, actor)
}

func (s *Service) UnpublishSite(siteID uint, actor string) error {
	return s.guardedRuntimeMutation(siteID, func(tx Store) error {
		if err := tx.DeactivatePublishes(siteID); err != nil {
			return err
		}
		if err := tx.SetSitePublication(siteID, "draft", false, time.Now().Unix()); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "site_unpublished", nil)
	})
}

func (s *Service) buildPublishFiles(draft PublicationDraft, site fallbackdomain.Site) ([]fallbackdomain.PublishFile, error) {
	pages := site.Pages
	if len(pages) == 0 {
		return nil, errors.New("site has no pages")
	}
	files := make([]fallbackdomain.PublishFile, 0, len(pages)+len(site.Assets)+3)
	hasCustomNotFound := false
	for _, page := range pages {
		if page.CanonicalPath == "/404.html" {
			hasCustomNotFound = true
		}
		var rendered []byte
		var err error
		if page.ContentMode == fallbackdomain.ContentModeStaticHTML {
			if err := fallbackdomain.ValidateStaticHTML(page.Body); err != nil {
				return nil, err
			}
			rendered = []byte(page.Body)
		} else {
			rendered, err = fallbackdomain.RenderGeneratedPage(fallbackdomain.TemplateData{
				TemplateID: site.TemplateID,
				SiteName:   site.Name,
				Title:      page.Title,
				Body:       page.Body,
				BodyIsHTML: page.ContentMode == fallbackdomain.ContentModeHTML,
				Pages:      pages,
			})
			if err != nil {
				return nil, err
			}
		}
		file, err := writePublishFile(draft, page.CanonicalPath, rendered, "text/html; charset=utf-8", htmlCachePolicy)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	var file fallbackdomain.PublishFile
	var err error
	if !hasCustomNotFound {
		notFound, err := fallbackdomain.RenderGeneratedPage(fallbackdomain.TemplateData{
			TemplateID: site.TemplateID,
			SiteName:   site.Name,
			Title:      "Page not found",
			Body:       "The page you requested is not available.",
			Pages:      pages,
		})
		if err != nil {
			return nil, err
		}
		file, err = writePublishFile(draft, "/404.html", notFound, "text/html; charset=utf-8", noStoreCachePolicy)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	for _, asset := range site.Assets {
		data, err := s.content.ReadAsset(asset)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != asset.Sha256 {
			return nil, fmt.Errorf("fallback-html asset %s failed integrity check", asset.LogicalPath)
		}
		file, err := writePublishFile(draft, asset.LogicalPath, data, asset.MimeType, assetCachePolicy)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	robots := []byte("User-agent: *\nDisallow:\n")
	file, err = writePublishFile(draft, "/robots.txt", robots, "text/plain; charset=utf-8", serviceCachePolicy)
	if err != nil {
		return nil, err
	}
	files = append(files, file)
	sitemap, err := buildSitemap(pages)
	if err != nil {
		return nil, err
	}
	file, err = writePublishFile(draft, "/sitemap.xml", sitemap, "application/xml; charset=utf-8", serviceCachePolicy)
	if err != nil {
		return nil, err
	}
	files = append(files, file)
	return files, nil
}

func buildSitemap(pages []fallbackdomain.Page) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	buf.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, page := range pages {
		buf.WriteString("  <url><loc>")
		if err := xml.EscapeText(&buf, []byte(page.CanonicalPath)); err != nil {
			return nil, err
		}
		buf.WriteString("</loc></url>\n")
	}
	buf.WriteString("</urlset>\n")
	return buf.Bytes(), nil
}

func buildPublishRedirects(redirects []fallbackdomain.Redirect) []fallbackdomain.PublishRedirect {
	out := make([]fallbackdomain.PublishRedirect, 0, len(redirects))
	for _, redirect := range redirects {
		out = append(out, fallbackdomain.PublishRedirect{
			FromPath:   redirect.FromPath,
			ToPath:     redirect.ToPath,
			StatusCode: redirect.StatusCode,
			External:   redirect.External,
		})
	}
	return out
}

func writePublishArtifact(draft PublicationDraft, version string, createdAt int64, site fallbackdomain.Site, targets []TargetView, external []ExternalResourceView, files []fallbackdomain.PublishFile, report SafetyReport) error {
	artifactFiles := make([]publishArtifactFile, 0, len(files))
	relativeByPublicPath := make(map[string]string, len(files))
	var checksums strings.Builder
	for _, file := range files {
		relative, err := filepath.Rel(draft.RootDir(), file.FilePath)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		artifactFiles = append(artifactFiles, publishArtifactFile{
			PublicPath:  file.PublicPath,
			Relative:    relative,
			MimeType:    file.MimeType,
			Sha256:      file.Sha256,
			SizeBytes:   file.SizeBytes,
			CachePolicy: file.CachePolicy,
		})
		relativeByPublicPath[file.PublicPath] = relative
		checksums.WriteString(file.Sha256)
		checksums.WriteByte(' ')
		checksums.WriteString(relative)
		checksums.WriteByte('\n')
	}
	assets := make([]AssetView, 0, len(site.Assets))
	for _, asset := range site.Assets {
		assets = append(assets, assetView(asset))
	}
	artifact := publishArtifact{
		Schema:    "solovey-ui/fallback-html-site/v1",
		Version:   version,
		CreatedAt: createdAt,
		Site: publishArtifactSite{
			ID:           site.ID,
			Name:         site.Name,
			TemplateID:   site.TemplateID,
			ExposureMode: site.ExposureMode,
			Hostname:     site.Hostname,
		},
		Targets:   targets,
		Pages:     site.Pages,
		Redirects: site.Redirects,
		Assets:    assets,
		External:  external,
		Files:     artifactFiles,
		Safety:    report,
	}
	if err := writeJSONFile(draft, ArtifactSite, artifact); err != nil {
		return err
	}
	targetArtifact := publishArtifactTargets{
		Schema:    "solovey-ui/fallback-html-targets/v1",
		Version:   version,
		CreatedAt: createdAt,
		Targets:   targets,
	}
	if err := writeJSONFile(draft, ArtifactTargets, targetArtifact); err != nil {
		return err
	}
	routesArtifact := buildRoutesArtifact(version, createdAt, site, relativeByPublicPath)
	if err := writeJSONFile(draft, ArtifactRoutes, routesArtifact); err != nil {
		return err
	}
	if err := writeJSONFile(draft, ArtifactSafety, report); err != nil {
		return err
	}
	if err := writeJSONFile(draft, ArtifactNode, buildNodeArtifactContract(version, createdAt, artifactFiles, report)); err != nil {
		return err
	}
	return draft.WriteArtifact(ArtifactChecksums, []byte(checksums.String()))
}

func buildRoutesArtifact(version string, createdAt int64, site fallbackdomain.Site, relativeByPublicPath map[string]string) publishArtifactRoutes {
	pages := make([]publishArtifactRoutePage, 0, len(site.Pages))
	canonicalMap := make(map[string]string, len(site.Pages)+len(site.Redirects))
	for _, page := range site.Pages {
		pages = append(pages, publishArtifactRoutePage{
			PublicPath: page.CanonicalPath,
			Relative:   relativeByPublicPath[page.CanonicalPath],
			Title:      page.Title,
			IsHome:     page.IsHome,
		})
		canonicalMap[page.Path] = page.CanonicalPath
	}
	redirects := make([]publishArtifactRouteRedirect, 0, len(site.Redirects))
	for _, redirect := range site.Redirects {
		redirects = append(redirects, publishArtifactRouteRedirect{
			FromPath:   redirect.FromPath,
			ToPath:     redirect.ToPath,
			StatusCode: redirect.StatusCode,
			External:   redirect.External,
		})
		canonicalMap[redirect.FromPath] = redirect.ToPath
	}
	return publishArtifactRoutes{
		Schema:       "solovey-ui/fallback-html-routes/v1",
		Version:      version,
		CreatedAt:    createdAt,
		Pages:        pages,
		Redirects:    redirects,
		CanonicalMap: canonicalMap,
	}
}

func (s *Service) buildPublishArchive(publish fallbackdomain.Publish) ([]byte, error) {
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	modTime := time.Unix(publish.CreatedAt, 0).UTC()

	metadata := []ArtifactPart{ArtifactSite, ArtifactTargets, ArtifactRoutes, ArtifactSafety, ArtifactNode, ArtifactChecksums}
	for _, relative := range metadata {
		data, err := s.content.ReadPublishMetadata(publish, relative)
		if err == nil {
			err = addArchiveFile(tarWriter, data, string(relative), modTime)
		}
		if err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return nil, err
		}
	}
	for _, file := range publish.Files {
		relative, err := filepath.Rel(publish.RootDir, file.FilePath)
		if err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return nil, err
		}
		relative = filepath.ToSlash(relative)
		data, err := s.content.ReadPublishFile(publish, file)
		if err == nil {
			err = addArchiveFile(tarWriter, data, relative, modTime)
		}
		if err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return nil, err
		}
	}
	if err := tarWriter.Close(); err != nil {
		_ = gzipWriter.Close()
		return nil, err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func addArchiveFile(writer *tar.Writer, data []byte, archiveName string, modTime time.Time) error {
	archiveName = filepath.ToSlash(strings.TrimSpace(archiveName))
	if err := validateArchiveName(archiveName); err != nil {
		return err
	}
	header := &tar.Header{
		Name:    archiveName,
		Mode:    0o640,
		Size:    int64(len(data)),
		ModTime: modTime,
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func validateArchiveName(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return fmt.Errorf("invalid fallback-html artifact path %q", value)
	}
	if value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.Contains(value, "/../") || strings.HasSuffix(value, "/..") {
		return fmt.Errorf("invalid fallback-html artifact path %q", value)
	}
	return nil
}

func SafeArchiveName(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
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
	if b.Len() == 0 {
		return "artifact"
	}
	return b.String()
}

func writeJSONFile(draft PublicationDraft, part ArtifactPart, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return draft.WriteArtifact(part, data)
}

func (s *Service) verifyPublishFiles(publish fallbackdomain.Publish) error {
	files := publish.Files
	if len(files) == 0 {
		return errors.New("publish has no files")
	}
	for _, file := range files {
		data, err := s.content.ReadPublishFile(publish, file)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != file.Sha256 {
			return fmt.Errorf("fallback-html publish file %s failed integrity check", file.PublicPath)
		}
	}
	return nil
}

func createSafetyReport(tx Store, siteID uint, report SafetyReport, now int64) error {
	warnings, err := json.Marshal(report.Warnings)
	if err != nil {
		return err
	}
	return tx.CreateSafetyReport(&fallbackdomain.SafetyReport{SiteID: siteID, OK: report.OK, Warnings: warnings, CreatedAt: now})
}

func writePublishFile(draft PublicationDraft, publicPath string, data []byte, mimeType string, cache string) (fallbackdomain.PublishFile, error) {
	diskPath, err := draft.WritePublicFile(publicPath, data)
	if err != nil {
		return fallbackdomain.PublishFile{}, err
	}
	sum := sha256.Sum256(data)
	return fallbackdomain.PublishFile{
		PublicPath:  publicPath,
		FilePath:    diskPath,
		MimeType:    mimeType,
		Sha256:      hex.EncodeToString(sum[:]),
		SizeBytes:   int64(len(data)),
		CachePolicy: cache,
	}, nil
}
