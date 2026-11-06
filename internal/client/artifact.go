package client

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/helper/identity"
)

var errArtifactMismatch = errors.New("different helper artifact")

type artifactStamp struct {
	Device, Inode     uint64
	Size              int64
	Mode, UID, GID    uint32
	Modified, Changed [2]int64
}

type artifactEntry struct {
	stamp  artifactStamp
	digest string
}

type artifactCache struct {
	mu    sync.Mutex
	files map[string]artifactEntry
}

func (cache *artifactCache) invalidate() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.files = nil
}

func (cache *artifactCache) check(path string, installed bool, expected identity.Identity, open func(string, bool) (*os.File, artifactStamp, error)) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	f, stamp, err := open(path, installed)
	if err != nil {
		return err
	}
	defer f.Close()
	entry, cached := cache.files[path]
	if !cached || entry.stamp != stamp {
		h := sha256.New()
		n, err := io.Copy(h, io.NewSectionReader(f, 0, stamp.Size))
		if err != nil || n != stamp.Size {
			return errors.New("helper artifact could not be read completely")
		}
		after, err := artifactMetadata(f, installed)
		if err != nil || stamp != after {
			return errors.New("helper artifact changed during verification")
		}
		entry = artifactEntry{stamp: stamp, digest: fmt.Sprintf("%x", h.Sum(nil))}
		if cache.files == nil || len(cache.files) >= 4 {
			cache.files = make(map[string]artifactEntry)
		}
		cache.files[path] = entry
	}
	if entry.digest != expected.SHA256 {
		return errArtifactMismatch
	}
	return nil
}

func untrustedHelper(detail string) *api.Error {
	return &api.Error{Code: api.ErrHelperUntrusted, Detail: detail}
}

func (c *Client) initIdentity() {
	if c.opts.ExpectedHelper != nil {
		c.expectedHelper = *c.opts.ExpectedHelper
		c.identityErr = c.expectedHelper.Validate()
	} else {
		c.expectedHelper, c.identityErr = identity.Parse([]byte(bundledHelperIdentity))
	}
	if c.expectedHelper.Protocol != helper.ProtocolVersion {
		c.identityErr = errors.New("bundled helper protocol is not supported by this client")
	}
	if c.opts.HelperPath != "" && (c.opts.ExpectedHelper == nil || !filepath.IsAbs(c.opts.HelperPath)) {
		c.identityErr = errors.New("explicit helper path requires a frozen expected identity")
	}
	c.binaryPath = helper.BinaryPath
	c.artifactOpen = openArtifact
	c.checkFile = func(path string, installed bool) error {
		return c.artifacts.check(path, installed, c.expectedHelper, c.artifactOpen)
	}
}

func (c *Client) bundledSource() (string, error) {
	if c.opts.HelperPath != "" {
		return c.opts.HelperPath, nil
	}
	if c.opts.ResourcesDir == "" {
		return "", errors.New("missing application resources")
	}
	return helperSource(c.opts.ResourcesDir)
}

func (c *Client) checkBundledHelper() (string, error) {
	if c.identityErr != nil {
		return "", untrustedHelper("bundled helper identity is unavailable; rebuild or reinstall Pith")
	}
	path, err := c.bundledSource()
	if err != nil || c.checkFile(path, false) != nil {
		return "", untrustedHelper("bundled helper does not match this app; reinstall Pith")
	}
	return path, nil
}

func (c *Client) checkHelperArtifacts() error {
	if _, err := c.checkBundledHelper(); err != nil {
		return err
	}
	err := c.checkFile(c.binaryPath, true)
	if errors.Is(err, os.ErrNotExist) {
		if _, socketErr := os.Lstat(c.opts.SocketPath); errors.Is(socketErr, os.ErrNotExist) {
			return helperMissing()
		}
		return untrustedHelper("helper socket exists without a verifiable installed artifact")
	}
	if errors.Is(err, errArtifactMismatch) {
		return &api.Error{Code: api.ErrHelperOutdated}
	}
	if err != nil {
		return untrustedHelper("installed helper metadata could not be verified")
	}
	return nil
}

func helperProblem(err error) (api.Status, bool) {
	code := api.Code(err)
	switch code {
	case api.ErrHelperMissing:
		return api.Status{State: api.Stopped}, true
	case api.ErrHelperOutdated, api.ErrHelperUntrusted:
		var problem *api.Error
		if !errors.As(err, &problem) {
			problem = &api.Error{Code: code}
		}
		return api.Status{State: api.Failed, Error: problem, Helper: api.HelperStatus{Installed: true, Outdated: code == api.ErrHelperOutdated}}, true
	default:
		return api.Status{}, false
	}
}
