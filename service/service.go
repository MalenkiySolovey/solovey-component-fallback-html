//go:build !minimal

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
)

type Service struct {
	db               Store
	runtime          *Runtime
	templateHTTP     *http.Client
	remoteCatalogURL string
	hostFacts        HostFacts
	templateMu       *sync.RWMutex
	content          Content
}

type SiteInput struct {
	ID         uint   `json:"id"`
	Name       string `json:"name"`
	Enabled    *bool  `json:"enabled"`
	TemplateID string `json:"templateId"`
}

type PageInput struct {
	ID          uint   `json:"id"`
	Path        string `json:"path"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	ContentMode string `json:"contentMode"`
	IsHome      bool   `json:"isHome"`
}

type PathValidationInput struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type PathValidationResult struct {
	OK               bool   `json:"ok"`
	CanonicalPath    string `json:"canonicalPath"`
	ReservedConflict bool   `json:"reservedConflict"`
	Message          string `json:"message"`
}

type RedirectInput struct {
	ID         uint   `json:"id"`
	FromPath   string `json:"fromPath"`
	ToPath     string `json:"toPath"`
	StatusCode int    `json:"statusCode"`
}

type ExternalResourceInput struct {
	ID      uint   `json:"id"`
	Kind    string `json:"kind"`
	URL     string `json:"url"`
	Allowed bool   `json:"allowed"`
}

type PreviewInput struct {
	PageID uint   `json:"pageId"`
	Path   string `json:"path"`
}

type PreviewResult struct {
	Path     string   `json:"path"`
	HTML     string   `json:"html"`
	Warnings []string `json:"warnings"`
}

// New composes the feature with the existing Backend owners. Remote requests
// use the Backend-approved client, including its SSRF and redirect policy.
func New(db Store, runtime *Runtime, facts HostFacts, content Content, templateHTTP *http.Client, catalogURL string) *Service {
	if runtime == nil || facts == nil || content == nil || templateHTTP == nil {
		panic("fallback-html Backend dependencies are required")
	}
	return &Service{db: db, runtime: runtime, hostFacts: facts, content: content, templateHTTP: templateHTTP, remoteCatalogURL: catalogURL, templateMu: &sync.RWMutex{}}
}
func (s *Service) withStore(db Store) *Service { scoped := *s; scoped.db = db; return &scoped }

func (s *Service) ListSites() ([]fallbackdomain.Site, error) {
	var sites []fallbackdomain.Site
	err := s.db.ListSites(&sites)
	return sites, err
}

func (s *Service) CreateSiteFromTemplate(templateID string, actor string) (fallbackdomain.Site, error) {
	if site, ok, err := s.createSiteFromInstalledTemplate(templateID, actor); ok || err != nil {
		return site, err
	}
	definition, err := templateDefinitionByID(templateID)
	if err != nil {
		return fallbackdomain.Site{}, err
	}
	return s.SaveSite(SiteInput{Name: definition.Name, TemplateID: definition.ID}, actor)
}

func (s *Service) RuntimeStatus() RuntimeStatus {
	return s.runtime.Status()
}

func (s *Service) SaveSite(input SiteInput, actor string) (fallbackdomain.Site, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = "Public Site"
	}
	templateID, err := s.normalizeTemplateID(input.TemplateID)
	if err != nil {
		return fallbackdomain.Site{}, err
	}
	now := time.Now().Unix()
	var site fallbackdomain.Site
	defaultTarget, err := s.currentWebTarget()
	if err != nil {
		return site, err
	}
	runtimeChanged := false
	err = s.db.WithMutationLock(func() error {
		transactionErr := s.db.Transaction(func(tx Store) error {
			if input.ID != 0 {
				if err := tx.LoadSite(input.ID, &site); err != nil {
					return err
				}
				if input.Enabled != nil && *input.Enabled != site.Enabled {
					if err := guardSiteTargetMutation(tx, site.ID); err != nil {
						return err
					}
					runtimeChanged = true
				}
			} else {
				site = fallbackdomain.Site{
					Enabled:      true,
					ExposureMode: "direct",
					Status:       "draft",
					CreatedAt:    now,
				}
			}
			site.Name = name
			site.TemplateID = templateID
			if input.Enabled != nil {
				site.Enabled = *input.Enabled
			}
			site.UpdatedAt = now
			if err := tx.SaveSite(&site); err != nil {
				return err
			}
			// GORM applies the model's true creation default to a false bool.
			// Retain an explicit disabled request in the same transaction.
			if input.ID == 0 && input.Enabled != nil && !*input.Enabled {
				if err := tx.SetSiteDisabled(site.ID); err != nil {
					return err
				}
				site.Enabled = false
			}
			if input.ID == 0 {
				if err := createDefaultPages(tx, site.ID, site.Name, now); err != nil {
					return err
				}
				target := targetFromView(site.ID, defaultTarget, now)
				if err := tx.CreateTarget(&target); err != nil {
					return err
				}
			}
			if err := RecordEvent(tx, site.ID, actor, "site_saved", map[string]any{"name": site.Name}); err != nil {
				return err
			}
			if runtimeChanged {
				return s.runtime.Rebuild(tx)
			}
			return nil
		})
		if transactionErr != nil && runtimeChanged {
			if restoreErr := s.runtime.Rebuild(s.db); restoreErr != nil {
				return errors.Join(transactionErr, fmt.Errorf("restore fallback-html runtime after rollback: %w", restoreErr))
			}
		}
		return transactionErr
	})
	if err != nil {
		return site, err
	}
	return s.GetSite(site.ID)
}

func (s *Service) normalizeTemplateID(value string) (string, error) {
	templateID := strings.TrimSpace(value)
	if templateID == "" {
		templateID = fallbackdomain.DefaultTemplateID
	}
	if _, err := templateDefinitionByID(templateID); err == nil {
		return templateID, nil
	}
	if _, ok, err := s.installedTemplatePackage(templateID); err != nil {
		return "", err
	} else if ok {
		return templateID, nil
	}
	var count int64
	if err := s.db.CountTemplateSites(templateID, &count); err != nil {
		return "", err
	}
	if count > 0 {
		return templateID, nil
	}
	return "", fmt.Errorf("unknown fallback-html template %q", templateID)
}

func templateDefinitionByID(templateID string) (fallbackdomain.TemplateDefinition, error) {
	return builtInTemplateDefinition(templateID)
}

func (s *Service) GetSite(id uint) (fallbackdomain.Site, error) {
	var site fallbackdomain.Site
	err := s.db.LoadSiteDetails(id, &site)
	return site, err
}

func (s *Service) ListPages(siteID uint) ([]fallbackdomain.Page, error) {
	var pages []fallbackdomain.Page
	err := s.db.ListPages(siteID, &pages)
	return pages, err
}

func (s *Service) DeleteSite(id uint, actor string) error {
	if err := s.guardedRuntimeMutation(id, func(tx Store) error {
		if err := RecordEvent(tx, id, actor, "site_deleted", nil); err != nil {
			return err
		}
		return tx.DeleteSite(id)
	}); err != nil {
		return err
	}
	return s.content.RemoveSite(id)
}

func (s *Service) ValidatePath(siteID uint, input PathValidationInput) (PathValidationResult, error) {
	reserved, err := s.reservedPaths()
	if err != nil {
		return PathValidationResult{}, err
	}
	canonical, err := fallbackdomain.ValidatePagePath(input.Path, reserved)
	result := PathValidationResult{CanonicalPath: canonical}
	if err != nil {
		normalized, normalizeErr := fallbackdomain.NormalizePublicPath(input.Path)
		if normalizeErr == nil {
			result.CanonicalPath = normalized
			result.ReservedConflict = fallbackdomain.IsReservedPublicPath(normalized, reserved)
		}
		result.Message = err.Error()
		return result, nil
	}
	kind := strings.ToLower(strings.TrimSpace(input.Kind))
	if kind == "" {
		kind = "page"
	}
	var conflicts int64
	switch kind {
	case "page":
		err = s.db.CountRedirectPath(siteID, canonical, &conflicts)
	case "redirect":
		err = s.db.CountPagePath(siteID, canonical, &conflicts)
	default:
		result.Message = "path kind must be page or redirect"
		return result, nil
	}
	if err != nil {
		return PathValidationResult{}, err
	}
	if conflicts > 0 {
		result.Message = "path is already used by another public route"
		return result, nil
	}
	result.OK = true
	return result, nil
}

func (s *Service) SavePage(siteID uint, input PageInput, actor string) (fallbackdomain.Page, error) {
	reserved, err := s.reservedPaths()
	if err != nil {
		return fallbackdomain.Page{}, err
	}
	canonical, err := fallbackdomain.ValidatePagePath(input.Path, reserved)
	if err != nil {
		return fallbackdomain.Page{}, err
	}
	title := strings.TrimSpace(input.Title)
	if title == "" {
		title = "Page"
	}
	body := strings.TrimSpace(input.Body)
	if body == "" {
		body = "This page is available."
	}
	contentMode, err := fallbackdomain.NormalizeContentMode(input.ContentMode)
	if err != nil {
		return fallbackdomain.Page{}, err
	}
	if contentMode == fallbackdomain.ContentModeHTML {
		sanitized, err := fallbackdomain.SanitizeBodyHTML(body)
		if err != nil {
			return fallbackdomain.Page{}, err
		}
		body = string(sanitized)
	}
	if contentMode == fallbackdomain.ContentModeStaticHTML {
		if err := fallbackdomain.ValidateStaticHTML(body); err != nil {
			return fallbackdomain.Page{}, err
		}
	}
	now := time.Now().Unix()
	var page fallbackdomain.Page
	err = s.db.Transaction(func(tx Store) error {
		if err := tx.LoadSite(siteID, &fallbackdomain.Site{}); err != nil {
			return err
		}
		var redirectCount int64
		if err := tx.CountRedirectPath(siteID, canonical, &redirectCount); err != nil {
			return err
		}
		if redirectCount > 0 {
			return fmt.Errorf("page path %s is already a redirect", canonical)
		}
		if input.ID != 0 {
			if err := tx.LoadPage(siteID, input.ID, &page); err != nil {
				return err
			}
		} else {
			page = fallbackdomain.Page{SiteID: siteID, Provenance: "generated", CreatedAt: now}
		}
		page.Path = canonical
		page.CanonicalPath = canonical
		page.Title = title
		page.Body = body
		page.ContentMode = contentMode
		page.IsHome = input.IsHome || canonical == "/"
		page.UpdatedAt = now
		if err := tx.SavePage(&page); err != nil {
			return err
		}
		if page.IsHome {
			if err := tx.ClearOtherHomePages(siteID, page.ID); err != nil {
				return err
			}
		}
		return RecordEvent(tx, siteID, actor, "page_saved", map[string]any{"path": canonical})
	})
	return page, err
}

func (s *Service) DeletePage(siteID, pageID uint, actor string) error {
	err := s.db.Transaction(func(tx Store) error {
		var count int64
		if err := tx.CountPages(siteID, &count); err != nil {
			return err
		}
		if count <= 1 {
			return fmt.Errorf("site must keep at least one page")
		}
		if err := tx.DeletePage(siteID, pageID); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "page_deleted", map[string]any{"pageId": pageID})
	})
	return err
}

func (s *Service) ListRedirects(siteID uint) ([]fallbackdomain.Redirect, error) {
	var redirects []fallbackdomain.Redirect
	err := s.db.ListRedirects(siteID, &redirects)
	return redirects, err
}

func (s *Service) SaveRedirect(siteID uint, input RedirectInput, actor string) (fallbackdomain.Redirect, error) {
	reserved, err := s.reservedPaths()
	if err != nil {
		return fallbackdomain.Redirect{}, err
	}
	fromPath, err := fallbackdomain.ValidatePagePath(input.FromPath, reserved)
	if err != nil {
		return fallbackdomain.Redirect{}, err
	}
	toPath, external, err := fallbackdomain.NormalizeRedirectTarget(input.ToPath, reserved)
	if err != nil {
		return fallbackdomain.Redirect{}, err
	}
	if !external && fromPath == toPath {
		return fallbackdomain.Redirect{}, fmt.Errorf("redirect source and target must differ")
	}
	statusCode, err := fallbackdomain.ValidateRedirectStatus(input.StatusCode)
	if err != nil {
		return fallbackdomain.Redirect{}, err
	}
	now := time.Now().Unix()
	var redirect fallbackdomain.Redirect
	err = s.db.Transaction(func(tx Store) error {
		if err := tx.LoadSite(siteID, &fallbackdomain.Site{}); err != nil {
			return err
		}
		var pageCount int64
		if err := tx.CountPagePath(siteID, fromPath, &pageCount); err != nil {
			return err
		}
		if pageCount > 0 {
			return fmt.Errorf("redirect source %s is already a page", fromPath)
		}
		if input.ID != 0 {
			if err := tx.LoadRedirect(siteID, input.ID, &redirect); err != nil {
				return err
			}
		} else {
			redirect = fallbackdomain.Redirect{SiteID: siteID, CreatedAt: now}
		}
		var duplicateCount int64
		if err := tx.CountOtherRedirectPath(siteID, fromPath, redirect.ID, &duplicateCount); err != nil {
			return err
		}
		if duplicateCount > 0 {
			return fmt.Errorf("redirect source %s already exists", fromPath)
		}
		redirect.FromPath = fromPath
		redirect.ToPath = toPath
		redirect.External = external
		redirect.StatusCode = statusCode
		redirect.UpdatedAt = now
		if err := tx.SaveRedirect(&redirect); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "redirect_saved", map[string]any{"from": fromPath, "external": external})
	})
	return redirect, err
}

func (s *Service) DeleteRedirect(siteID, redirectID uint, actor string) error {
	return s.db.Transaction(func(tx Store) error {
		if err := tx.DeleteRedirect(siteID, redirectID); err != nil {
			return err
		}
		return RecordEvent(tx, siteID, actor, "redirect_deleted", map[string]any{"redirectId": redirectID})
	})
}

func (s *Service) Safety(siteID uint) (SafetyReport, error) {
	site, err := s.GetSite(siteID)
	if err != nil {
		return SafetyReport{}, err
	}
	return s.safetyForSite(site)
}

func (s *Service) PreviewSite(siteID uint, input PreviewInput, actor string) (PreviewResult, error) {
	site, err := s.GetSite(siteID)
	if err != nil {
		return PreviewResult{}, err
	}
	if len(site.Pages) == 0 {
		return PreviewResult{}, errors.New("site has no pages")
	}
	page := site.Pages[0]
	if input.PageID != 0 {
		found := false
		for _, candidate := range site.Pages {
			if candidate.ID == input.PageID {
				page = candidate
				found = true
				break
			}
		}
		if !found {
			return PreviewResult{}, fmt.Errorf("preview page not found")
		}
	} else if strings.TrimSpace(input.Path) != "" {
		canonical, err := fallbackdomain.NormalizePublicPath(input.Path)
		if err != nil {
			return PreviewResult{}, err
		}
		found := false
		for _, candidate := range site.Pages {
			if candidate.CanonicalPath == canonical {
				page = candidate
				found = true
				break
			}
		}
		if !found {
			return PreviewResult{}, fmt.Errorf("preview page %s not found", canonical)
		}
	} else {
		for _, candidate := range site.Pages {
			if candidate.IsHome || candidate.CanonicalPath == "/" {
				page = candidate
				break
			}
		}
	}
	var rendered []byte
	if page.ContentMode == fallbackdomain.ContentModeStaticHTML {
		if err := fallbackdomain.ValidateStaticHTML(page.Body); err != nil {
			return PreviewResult{}, err
		}
		rendered = []byte(page.Body)
		rendered, err = s.inlinePreviewStyles(site.ID, rendered)
		if err != nil {
			return PreviewResult{}, err
		}
	} else {
		rendered, err = fallbackdomain.RenderGeneratedPage(fallbackdomain.TemplateData{
			TemplateID: site.TemplateID,
			SiteName:   site.Name,
			Title:      page.Title,
			Body:       page.Body,
			BodyIsHTML: page.ContentMode == fallbackdomain.ContentModeHTML,
			Pages:      site.Pages,
		})
		if err != nil {
			return PreviewResult{}, err
		}
	}
	report, err := s.safetyForSite(site)
	if err != nil {
		return PreviewResult{}, err
	}
	if err := RecordEvent(s.db, siteID, actor, "site_previewed", map[string]any{"path": page.CanonicalPath}); err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{Path: page.CanonicalPath, HTML: string(rendered), Warnings: report.Warnings}, nil
}

func (s *Service) inlinePreviewStyles(siteID uint, html []byte) ([]byte, error) {
	var assets []fallbackdomain.Asset
	if err := s.db.ListStyleAssets(siteID, &assets); err != nil {
		return nil, err
	}
	out := string(html)
	for _, asset := range assets {
		if asset.LogicalPath == "" || asset.FilePath == "" {
			continue
		}
		data, err := s.content.ReadAsset(asset)
		if err != nil {
			return nil, err
		}
		css := strings.ReplaceAll(string(data), "</style", "<\\/style")
		style := "<style data-fallback-preview=\"" + asset.LogicalPath + "\">\n" + css + "\n</style>"
		out = replaceStylesheetLink(out, asset.LogicalPath, style)
	}
	return []byte(out), nil
}

func replaceStylesheetLink(html string, href string, style string) string {
	linkTag := regexp.MustCompile(`(?is)<link\b[^>]*>`)
	return linkTag.ReplaceAllStringFunc(html, func(tag string) string {
		lower := strings.ToLower(tag)
		if !strings.Contains(lower, "stylesheet") {
			return tag
		}
		if !strings.Contains(tag, `href="`+href+`"`) && !strings.Contains(tag, `href='`+href+`'`) {
			return tag
		}
		return style
	})
}

func (s *Service) reservedPaths() ([]string, error) {
	if s.hostFacts == nil {
		return nil, errors.New("host facts are unavailable")
	}
	return s.hostFacts.ReservedPaths(context.Background())
}

func (s *Service) safetyForSite(site fallbackdomain.Site) (SafetyReport, error) {
	var warnings []string
	reserved, err := s.reservedPaths()
	webPath := ""
	if len(reserved) > 0 {
		webPath = reserved[0]
	}
	if err != nil {
		return SafetyReport{}, err
	}
	if webPath == "/" {
		warnings = append(warnings, "admin web path is '/', move the panel to a secret path before publishing")
	}
	if webPath == "/app/" {
		warnings = append(warnings, "admin web path is still the default /app/")
	}
	hasHome := false
	for _, page := range site.Pages {
		if page.IsHome || page.CanonicalPath == "/" {
			hasHome = true
		}
		if fallbackdomain.IsReservedPublicPath(page.CanonicalPath, reserved) {
			warnings = append(warnings, "page "+page.CanonicalPath+" conflicts with reserved panel paths")
		}
	}
	for _, redirect := range site.Redirects {
		if fallbackdomain.IsReservedPublicPath(redirect.FromPath, reserved) {
			warnings = append(warnings, "redirect "+redirect.FromPath+" conflicts with reserved panel paths")
		}
		if !redirect.External && fallbackdomain.IsReservedPublicPath(redirect.ToPath, reserved) {
			warnings = append(warnings, "redirect "+redirect.FromPath+" targets a reserved panel path")
		}
	}
	if !hasHome {
		warnings = append(warnings, "site does not have a home page at /")
	}
	targets, err := s.targetsForSite(site)
	if err != nil {
		return SafetyReport{}, err
	}
	for _, target := range targets {
		switch target.Status {
		case "", "available", "managed", "free", "active":
		default:
			warnings = append(warnings, "publish target "+target.Kind+" is "+target.Status+": "+target.Reason)
		}
		if target.Kind == "web-current" {
			if fallback, reason := s.hostFacts.AddressFallback(context.Background(), target.Listen); fallback {
				warnings = append(warnings, "publish target would use loopback fallback: "+reason)
			}
		}
	}
	return SafetyReport{OK: len(warnings) == 0, Warnings: warnings}, nil
}

func createDefaultPages(tx Store, siteID uint, siteName string, now int64) error {
	pages := []fallbackdomain.Page{
		{SiteID: siteID, Path: "/", CanonicalPath: "/", Title: siteName, Body: "A simple public site is available here.", ContentMode: "text", IsHome: true, SortOrder: 1, Provenance: "generated", CreatedAt: now, UpdatedAt: now},
		{SiteID: siteID, Path: "/about/", CanonicalPath: "/about/", Title: "About", Body: "This page keeps a normal public surface on this host.", ContentMode: "text", SortOrder: 2, Provenance: "generated", CreatedAt: now, UpdatedAt: now},
	}
	return tx.CreatePages(&pages)
}

func RecordEvent(tx Store, siteID uint, actor string, action string, details map[string]any) error {
	raw := json.RawMessage(nil)
	if details != nil {
		data, err := json.Marshal(details)
		if err != nil {
			return err
		}
		raw = data
	}
	return tx.CreateEvent(&fallbackdomain.Event{SiteID: siteID, Actor: actor, Action: action, Details: raw, CreatedAt: time.Now().Unix()})
}

func uintString(value uint) string {
	return strconv.FormatUint(uint64(value), 10)
}
