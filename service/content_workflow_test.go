//go:build !minimal

package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	fallbackdomain "github.com/MalenkiySolovey/solovey-component-fallback-html/domain"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type assetTestStore struct {
	Store
	asset                 *fallbackdomain.Asset
	failEvent, failCommit bool
	used                  int64
}

func (s *assetTestStore) Transaction(call func(Store) error) error {
	prior := s.asset
	err := call(s)
	if err == nil && s.failCommit {
		err = errors.New("commit rejected")
	}
	if err != nil {
		s.asset = prior
	}
	return err
}
func (s *assetTestStore) LoadSite(uint, *fallbackdomain.Site) error { return nil }
func (s *assetTestStore) LoadAssetPath(_ uint, _ string, out *fallbackdomain.Asset) error {
	if s.asset == nil {
		return ErrRecordNotFound
	}
	*out = *s.asset
	return nil
}
func (s *assetTestStore) LoadAsset(siteID, id uint, out *fallbackdomain.Asset) error {
	if s.asset == nil || s.asset.ID != id || s.asset.SiteID != siteID {
		return ErrRecordNotFound
	}
	*out = *s.asset
	return nil
}
func (s *assetTestStore) AssetBytesUsed(_ uint, out *int64) error { *out = s.used; return nil }
func (s *assetTestStore) CreateAsset(a *fallbackdomain.Asset) error {
	a.ID = 11
	copy := *a
	s.asset = &copy
	return nil
}
func (s *assetTestStore) DeleteAsset(*fallbackdomain.Asset) error { s.asset = nil; return nil }
func (s *assetTestStore) CreateEvent(*fallbackdomain.Event) error {
	if s.failEvent {
		return errors.New("audit rejected")
	}
	return nil
}
func (s *assetTestStore) Context() context.Context { return context.Background() }

type memoryContent struct {
	Content
	files                          map[string][]byte
	removed, rolledBack, committed int
}

func (c *memoryContent) ReadAsset(a fallbackdomain.Asset) ([]byte, error) {
	b, ok := c.files[a.FilePath]
	if !ok {
		return nil, errors.New("asset missing")
	}
	return append([]byte(nil), b...), nil
}
func (c *memoryContent) CreateAsset(_ uint, name string, data []byte) (string, error) {
	p := "asset/" + name
	c.files[p] = append([]byte(nil), data...)
	return p, nil
}
func (c *memoryContent) RemoveCreatedAsset(_ uint, p string) error {
	delete(c.files, p)
	c.removed++
	return nil
}

type memoryChange struct{ commit, rollback func() error }

func (c memoryChange) Commit() error   { return c.commit() }
func (c memoryChange) Rollback() error { return c.rollback() }
func (c *memoryContent) StageAssetRemoval(a fallbackdomain.Asset) (ContentChange, error) {
	old, ok := c.files[a.FilePath]
	if !ok {
		return memoryChange{func() error { return nil }, func() error { return nil }}, nil
	}
	delete(c.files, a.FilePath)
	return memoryChange{func() error { c.committed++; return nil }, func() error { c.files[a.FilePath] = old; c.rolledBack++; return nil }}, nil
}
func TestAssetWorkflowCompensatesAuditAndCommitFailures(t *testing.T) {
	for _, commitFailure := range []bool{false, true} {
		store := &assetTestStore{failEvent: !commitFailure, failCommit: commitFailure}
		content := &memoryContent{files: map[string][]byte{}}
		s := &Service{db: store, content: content}
		if _, err := s.SaveAsset(7, "style.css", []byte("body{color:red}"), "operator"); err == nil {
			t.Fatal("transaction failure accepted")
		}
		if len(content.files) != 0 || store.asset != nil || content.removed != 1 {
			t.Fatal("asset file survived failed transaction")
		}
		store.failEvent = false
		store.failCommit = false
		asset, err := s.SaveAsset(7, "style.css", []byte("body{color:red}"), "operator")
		if err != nil {
			t.Fatal(err)
		}
		store.failEvent = true
		if err := s.DeleteAsset(7, asset.ID, "operator"); err == nil {
			t.Fatal("delete audit failure accepted")
		}
		if store.asset == nil || len(content.files) != 1 || content.rolledBack != 1 {
			t.Fatal("delete compensation lost asset")
		}
		store.failEvent = false
		if err := s.DeleteAsset(7, asset.ID, "operator"); err != nil {
			t.Fatal(err)
		}
		if store.asset != nil || len(content.files) != 0 || content.committed != 1 {
			t.Fatal("committed deletion retained asset")
		}
	}
}
func TestAssetQuotaAndExistingHashAreVerifiedBeforeEffects(t *testing.T) {
	store := &assetTestStore{used: maxSiteAssetBytes}
	content := &memoryContent{files: map[string][]byte{}}
	s := &Service{db: store, content: content}
	if _, err := s.SaveAsset(7, "style.css", []byte("body{}"), "operator"); err == nil {
		t.Fatal("quota bypassed")
	}
	if len(content.files) > 0 {
		t.Fatal("quota check wrote a file")
	}
	store.used = 0
	if _, err := s.SaveAsset(7, "style.css", []byte("body{}"), "operator"); err != nil {
		t.Fatal(err)
	}
	content.files[store.asset.FilePath] = []byte("corrupted")
	if _, err := s.SaveAsset(7, "style.css", []byte("body{}"), "operator"); err == nil {
		t.Fatal("existing corrupt asset accepted")
	}
}

type memoryPublication struct {
	root  string
	files map[string][]byte
}

func (d *memoryPublication) RootDir() string { return d.root }
func (d *memoryPublication) WritePublicFile(p string, data []byte) (string, error) {
	name := filepath.Join(d.root, fallbackdomain.PublicPathToFilePath(p))
	if _, exists := d.files[name]; exists {
		return "", errors.New("file exists")
	}
	d.files[name] = append([]byte(nil), data...)
	return name, nil
}
func (d *memoryPublication) WriteArtifact(p ArtifactPart, data []byte) error {
	if !p.Valid() {
		return errors.New("invalid artifact")
	}
	d.files[filepath.Join(d.root, string(p))] = append([]byte(nil), data...)
	return nil
}
func (d *memoryPublication) Discard() error { d.files = map[string][]byte{}; return nil }

type archiveContent struct {
	Content
	files map[string][]byte
}

func (c archiveContent) ReadPublishFile(_ fallbackdomain.Publish, f fallbackdomain.PublishFile) ([]byte, error) {
	d, ok := c.files[f.FilePath]
	if !ok {
		return nil, errors.New("publish file missing")
	}
	return d, nil
}
func (c archiveContent) ReadPublishMetadata(p fallbackdomain.Publish, a ArtifactPart) ([]byte, error) {
	d, ok := c.files[filepath.Join(p.RootDir, string(a))]
	if !ok {
		return nil, errors.New("metadata missing")
	}
	return d, nil
}
func TestPublicationRenderingMetadataAndArchiveRetainTheirBytes(t *testing.T) {
	data := []byte("body{color:red}")
	sum := sha256.Sum256(data)
	asset := fallbackdomain.Asset{SiteID: 7, LogicalPath: "/media/site.css", FilePath: "asset/site.css", MimeType: "text/css; charset=utf-8", Sha256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(data))}
	site := fallbackdomain.Site{ID: 7, Name: "Portal", TemplateID: fallbackdomain.DefaultTemplateID, Pages: []fallbackdomain.Page{{Path: "/", CanonicalPath: "/", Title: "Home", Body: "Safe body", ContentMode: "text", IsHome: true}}, Assets: []fallbackdomain.Asset{asset}}
	s := &Service{content: &memoryContent{files: map[string][]byte{asset.FilePath: data}}}
	draft := &memoryPublication{root: "publish-revision", files: map[string][]byte{}}
	files, err := s.buildPublishFiles(draft, site)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("files=%d", len(files))
	}
	report := SafetyReport{OK: true}
	if err := writePublishArtifact(draft, "revision", 1700000000, site, nil, nil, files, report); err != nil {
		t.Fatal(err)
	}
	publish := fallbackdomain.Publish{SiteID: 7, Version: "revision", RootDir: draft.root, CreatedAt: 1700000000, Files: files}
	s.content = archiveContent{files: draft.files}
	if err := s.verifyPublishFiles(publish); err != nil {
		t.Fatal(err)
	}
	archive, err := s.buildPublishArchive(publish)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		h, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, draft.files[filepath.Join(draft.root, h.Name)]) {
			t.Fatalf("archive bytes differ for %s", h.Name)
		}
		if h.Mode != 0o640 || h.ModTime.Unix() != publish.CreatedAt {
			t.Fatalf("archive metadata=%#v", h)
		}
		seen[h.Name] = true
	}
	if len(seen) != 11 || !seen["site.json"] || !seen["checksums.txt"] {
		t.Fatalf("archive entries=%#v", seen)
	}
	if !strings.Contains(string(draft.files[filepath.Join(draft.root, "index.html")]), "Safe body") {
		t.Fatal("page body missing")
	}
	for _, f := range files {
		if f.PublicPath == "/media/site.css" {
			draft.files[f.FilePath] = []byte("tampered")
		}
	}
	if err := s.verifyPublishFiles(publish); err == nil {
		t.Fatal("tampered publication accepted")
	}
}
