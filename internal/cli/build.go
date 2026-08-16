package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// Building an image instead of pulling one.
//
// The tag is a hash of what went into it, which is the whole design: an unchanged context
// produces the same tag, the image is already there, and the build is skipped entirely.
// Daytona caches builds for 24 hours; a clock is the wrong key for this - it rebuilds work
// that has not changed and reuses work that has.
//
// Content, not timestamps. A fresh `git clone` rewrites every mtime and would miss every
// cache entry on a CI runner, which is precisely where the cache is worth the most.

// BuildTag is the image name a build produces: a hash of its context and Dockerfile.
func BuildTag(specDir string, b *spec.Build) (string, error) {
	root := filepath.Join(specDir, b.Context)

	sum, err := hashDir(root)
	if err != nil {
		return "", err
	}

	return "sbx-build-" + sum[:16] + ":latest", nil
}

// hashDir digests every file under root: path, mode and contents, in a stable order.
//
// Sorted, because filepath.WalkDir's order is stable but the hash should not depend on that
// promise. Mode is in it because a script that stops being executable is a different image
// and would otherwise be a silent cache hit.
func hashDir(root string) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("build context %s: %w", root, err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("build context %s is not a directory", root)
	}

	var paths []string

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			// Nothing here belongs in an image, and .git in particular would make the tag
			// change on every commit whether or not the build inputs did.
			if name := d.Name(); name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}

			return nil
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return nil // a link's target is either in the context already or outside it
		}

		paths = append(paths, p)

		return nil
	})
	if err != nil {
		return "", err
	}

	sort.Strings(paths)

	h := sha256.New()

	for _, p := range paths {
		rel, _ := filepath.Rel(root, p)

		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}

		fmt.Fprintf(h, "%s\x00%o\x00", filepath.ToSlash(rel), st.Mode().Perm())

		f, err := os.Open(p)
		if err != nil {
			return "", err
		}

		_, err = io.Copy(h, f)
		f.Close()

		if err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// buildIfNeeded resolves a service's build into an image, building only when the resulting
// tag is not already present.
func buildIfNeeded(ctx context.Context, p provider.Provider, specDir, name string, svc spec.Service) (spec.Service, error) {
	if svc.Build == nil {
		return svc, nil
	}

	b, err := provider.BuilderFor(p)
	if err != nil {
		return svc, err
	}

	tag, err := BuildTag(specDir, svc.Build)
	if err != nil {
		return svc, err
	}

	have, err := b.HasImage(ctx, tag)
	if err != nil {
		return svc, err
	}

	if have {
		fmt.Printf("  %-12s build cached (%s)\n", name, strings.TrimSuffix(tag, ":latest"))

		svc.Image = tag

		return svc, nil
	}

	fmt.Printf("  %-12s building...\n", name)

	dockerfile := svc.Build.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}

	if err := b.Build(ctx, tag, filepath.Join(specDir, svc.Build.Context), dockerfile); err != nil {
		return svc, fmt.Errorf("service %q: %w", name, err)
	}

	svc.Image = tag

	return svc, nil
}
