//go:build !minimal

package service

import (
	"context"
	"errors"
	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
)

var ErrRecordNotFound = errors.New("fallback-html record not found")

// Store contains only existing Fallback data operations. Its Backend adapter
// owns the connection, transaction, reservation lock and candidate/live binding.
// It has no SQL, arbitrary table, Settings or cross-owner mutation operation.
type Store interface {
	PublicationSource
	Context() context.Context
	WithContext(context.Context) Store
	Transaction(func(Store) error) error
	WithMutationLock(func() error) error
	WithMutationLockContext(context.Context, func() error) error
	GuardSiteMutation(uint) error
	ListSites(out *[]fallbackdomain.Site) error
	LoadSite(id uint, out *fallbackdomain.Site) error
	LoadSiteDetails(id uint, out *fallbackdomain.Site) error
	LoadSiteTargets(id uint, out *fallbackdomain.Site) error
	SaveSite(site *fallbackdomain.Site) error
	CreateSite(site *fallbackdomain.Site) error
	SetSiteDisabled(id uint) error
	DeleteSite(id uint) error
	CountTemplateSites(templateID string, out *int64) error
	ListPages(siteID uint, out *[]fallbackdomain.Page) error
	LoadPage(siteID, id uint, out *fallbackdomain.Page) error
	SavePage(page *fallbackdomain.Page) error
	CreatePages(pages *[]fallbackdomain.Page) error
	ClearOtherHomePages(siteID, exceptID uint) error
	CountPages(siteID uint, out *int64) error
	CountPagePath(siteID uint, canonical string, out *int64) error
	DeletePage(siteID, id uint) error
	DeletePages(siteID uint) error
	ListRedirects(siteID uint, out *[]fallbackdomain.Redirect) error
	LoadRedirect(siteID, id uint, out *fallbackdomain.Redirect) error
	SaveRedirect(redirect *fallbackdomain.Redirect) error
	CreateRedirects(redirects *[]fallbackdomain.Redirect) error
	CountRedirectPath(siteID uint, canonical string, out *int64) error
	CountOtherRedirectPath(siteID uint, canonical string, exceptID uint, out *int64) error
	DeleteRedirect(siteID, id uint) error
	DeleteRedirects(siteID uint) error
	ListAssets(siteID uint, out *[]fallbackdomain.Asset) error
	ListStyleAssets(siteID uint, out *[]fallbackdomain.Asset) error
	LoadAssetPath(siteID uint, logicalPath string, out *fallbackdomain.Asset) error
	LoadAsset(siteID, id uint, out *fallbackdomain.Asset) error
	CreateAsset(asset *fallbackdomain.Asset) error
	DeleteAsset(asset *fallbackdomain.Asset) error
	AssetBytesUsed(siteID uint, out *int64) error
	ListExternalResources(siteID uint, out *[]fallbackdomain.ExternalResource) error
	LoadExternalResource(siteID, id uint, out *fallbackdomain.ExternalResource) error
	SaveExternalResource(resource *fallbackdomain.ExternalResource) error
	DeleteExternalResource(siteID, id uint) error
	ListPublishes(siteID uint, out *[]fallbackdomain.Publish) error
	LoadPublish(siteID uint, version string, out *fallbackdomain.Publish) error
	DeactivatePublishes(siteID uint) error
	ActivatePublish(id uint) error
	CreatePublish(publish *fallbackdomain.Publish) error
	CreatePublishFiles(files *[]fallbackdomain.PublishFile) error
	CreatePublishRedirects(redirects *[]fallbackdomain.PublishRedirect) error
	DeletePublishFiles(ids []uint) error
	DeletePublishRedirects(ids []uint) error
	DeleteInactivePublishes(siteID uint, ids []uint) error
	PublishedSiteConflict(siteID uint, out *fallbackdomain.Site) error
	CreateSafetyReport(report *fallbackdomain.SafetyReport) error
	CreateEvent(event *fallbackdomain.Event) error
	LoadTemplateSource(templateID string, out *fallbackdomain.TemplateSource) error
	ListTemplateSources(out *[]fallbackdomain.TemplateSource) error
	UpsertTemplateSource(values fallbackdomain.TemplateSource, createdAt int64, out *fallbackdomain.TemplateSource) error
	DeleteTemplateSource(source *fallbackdomain.TemplateSource) error
	ResetTemplateSource(id uint, definition fallbackdomain.TemplateDefinition, now int64) error
	LoadTarget(siteID, id uint, out *fallbackdomain.RuntimeTarget) error
	SaveTarget(target *fallbackdomain.RuntimeTarget) error
	CreateTarget(target *fallbackdomain.RuntimeTarget) error
	DeleteTarget(target *fallbackdomain.RuntimeTarget) error
	DeleteOtherTargets(siteID, exceptID uint) error
	ListSavedTargets(out *[]fallbackdomain.RuntimeTarget) error
	SetSitePublication(siteID uint, status string, clearError bool, now int64) error
	LoadRollbackPublish(siteID uint, version string, out *fallbackdomain.Publish) error
}
