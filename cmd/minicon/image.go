package main

// image.go -- the "image format" for minicon.
//
// A minicon image is just a gzip-compressed tar archive with:
//   - manifest.json   (metadata: entrypoint, env, exposed port)
//   - rootfs/...       (every file the app needs: the binary + its libraries)
//
// This mirrors how a real OCI/Docker image is a manifest + layered tarballs,
// stripped down to the essentials so the mechanism is easy to read.

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Manifest is the metadata stored inside the image.
type Manifest struct {
	Name       string   `json:"name"`       // image name, e.g. "hello:1.0"
	Entrypoint []string `json:"entrypoint"` // command to run inside the container
	Env        []string `json:"env"`        // extra environment variables (KEY=VALUE)
	Port       string   `json:"port"`       // the port the app listens on (for docs)
	CreatedAt  string   `json:"created_at"`
}

const manifestName = "manifest.json"
const rootfsPrefix = "rootfs"

// writeManifest serializes a manifest into a tar archive.
func writeManifest(tw *tar.Writer, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	hdr := &tar.Header{
		Name: manifestName,
		Mode: 0644,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

// addPathToTar walks a file or directory on disk and writes every entry into
// the tar archive under rootfs/<relative-path>.
func addPathToTar(tw *tar.Writer, srcPath, destRel string) error {
	return filepath.Walk(srcPath, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Build the path as it will appear inside the image.
		rel, err := filepath.Rel(srcPath, p)
		if err != nil {
			return err
		}
		name := filepath.Join(rootfsPrefix, destRel, rel)
		name = filepath.ToSlash(name)

		// Follow symlinks' targets by recording them as symlinks.
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}

		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		// Only regular files have content to copy.
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
}

// readImage opens an image file and returns its manifest plus a reader
// positioned so callers can extract the rootfs entries.
func extractImage(imagePath, destRootfs string) (Manifest, error) {
	var m Manifest
	f, err := os.Open(imagePath)
	if err != nil {
		return m, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return m, fmt.Errorf("not a gzip image: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	foundManifest := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, err
		}

		switch {
		case hdr.Name == manifestName:
			data, err := io.ReadAll(tr)
			if err != nil {
				return m, err
			}
			if err := json.Unmarshal(data, &m); err != nil {
				return m, err
			}
			foundManifest = true

		case strings.HasPrefix(hdr.Name, rootfsPrefix+"/"):
			rel := strings.TrimPrefix(hdr.Name, rootfsPrefix+"/")
			target := filepath.Join(destRootfs, rel)
			if err := extractEntry(tr, hdr, target); err != nil {
				return m, err
			}
		}
	}

	if !foundManifest {
		return m, fmt.Errorf("image has no %s", manifestName)
	}
	return m, nil
}

// extractEntry writes a single tar entry (dir, file, or symlink) to disk.
func extractEntry(tr *tar.Reader, hdr *tar.Header, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, os.FileMode(hdr.Mode))
	case tar.TypeSymlink:
		_ = os.MkdirAll(filepath.Dir(target), 0755)
		_ = os.Remove(target)
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, tr)
		return err
	default:
		return nil // skip other types for this simple version
	}
}
