// Package upload writes user-supplied images into the static directory.
//
// It is its own package rather than a file in util because util is documented as
// a leaf of pure helpers: this touches the filesystem, and mixing the two would
// make every importer of a formatting function depend on disk IO.
package upload

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var (
	// ErrTooLarge reports a file over the configured cap.
	ErrTooLarge = errors.New("upload: file exceeds the size limit")
	// ErrUnsupportedType reports content that is not one of the allowed image
	// formats, whatever the filename claimed.
	ErrUnsupportedType = errors.New("upload: unsupported image type")
)

// sniffLen is what http.DetectContentType reads. Its documented maximum.
const sniffLen = 512

// allowedTypes maps an accepted sniffed content type to the extension the stored
// file gets.
//
// The extension is derived from the sniffed bytes, never from the uploaded
// filename. Trusting the filename would mean maintaining a second list that can
// disagree with this one, and a disagreement is exactly the hole an upload
// whitelist exists to close.
var allowedTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

// ImageStore saves images below a root directory.
//
// root is the directory the static file server serves ("static") and subdir is
// the path within it ("uploads/services"). Save returns the path relative to
// root, which is what services.image_path stores and what the templates render
// as /static/<path> — so the database never holds a filesystem path.
type ImageStore struct {
	root     string
	subdir   string
	maxBytes int64
}

// New returns a store, creating the destination directory.
//
// Creating it here rather than on first use makes a directory that cannot be
// created a failed boot with a clear message, instead of an upload that fails
// for the first admin who tries one.
func New(root, subdir string, maxBytes int64) (*ImageStore, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("upload: maxBytes must be positive, got %d", maxBytes)
	}

	dir := filepath.Join(root, filepath.FromSlash(subdir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("upload: creating %s: %w", dir, err)
	}

	return &ImageStore{root: root, subdir: subdir, maxBytes: maxBytes}, nil
}

// MaxBytes is the per-file cap, which the handler also needs to bound the
// request body.
func (s *ImageStore) MaxBytes() int64 { return s.maxBytes }

// Save validates fh and writes it, returning the stored path relative to root,
// e.g. "uploads/services/1f0c….webp".
//
// The caller is responsible for deleting the returned file if whatever it does
// next fails — see Catalog, which writes the file, updates the row, and only
// then removes the image the row used to point at.
func (s *ImageStore) Save(fh *multipart.FileHeader) (string, error) {
	if fh == nil {
		return "", ErrUnsupportedType
	}
	// fh.Size is the declared length. It is checked first as a cheap rejection,
	// but it is not trusted: the copy below is bounded independently.
	if fh.Size > s.maxBytes {
		return "", ErrTooLarge
	}

	src, err := fh.Open()
	if err != nil {
		return "", fmt.Errorf("upload: opening submitted file: %w", err)
	}
	defer src.Close()

	head := make([]byte, sniffLen)
	n, err := io.ReadFull(src, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("upload: reading submitted file: %w", err)
	}

	ext, ok := allowedTypes[normaliseType(http.DetectContentType(head[:n]))]
	if !ok {
		return "", ErrUnsupportedType
	}

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("upload: rewinding submitted file: %w", err)
	}

	name, err := randomName(ext)
	if err != nil {
		return "", err
	}

	rel := path.Join(s.subdir, name)
	abs := filepath.Join(s.root, filepath.FromSlash(rel))

	// O_EXCL so a name collision fails loudly instead of overwriting another
	// service's image. With 16 random bytes it never happens; if it ever does,
	// the alternative is silent data loss.
	dst, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("upload: creating %s: %w", abs, err)
	}

	// LimitReader at maxBytes+1 so an oversized body is detected rather than
	// silently truncated to the cap. fh.Size cannot be relied on for this: it is
	// taken from the request, and a multipart part can carry more than it claims.
	written, err := io.Copy(dst, io.LimitReader(src, s.maxBytes+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil && written > s.maxBytes {
		err = ErrTooLarge
	}
	if err != nil {
		// Never leave a partial or oversized file behind.
		_ = os.Remove(abs)
		if errors.Is(err, ErrTooLarge) {
			return "", ErrTooLarge
		}
		return "", fmt.Errorf("upload: writing %s: %w", abs, err)
	}

	return rel, nil
}

// Remove deletes a path previously returned by Save. A path that is already gone
// is not an error: callers use this for best-effort cleanup of a replaced image,
// where a missing file is the desired end state anyway.
//
// Anything outside subdir is refused, so a corrupted or hand-edited image_path
// cannot turn a routine cleanup into a delete elsewhere on the filesystem.
func (s *ImageStore) Remove(rel string) error {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return nil
	}

	clean := path.Clean("/" + rel)[1:]
	if !strings.HasPrefix(clean, s.subdir+"/") {
		return fmt.Errorf("upload: refusing to remove %q outside %s", rel, s.subdir)
	}

	err := os.Remove(filepath.Join(s.root, filepath.FromSlash(clean)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// normaliseType drops any parameters from a sniffed content type, since
// DetectContentType returns "text/plain; charset=utf-8" for anything it does not
// recognise.
func normaliseType(ct string) string {
	base, _, _ := strings.Cut(ct, ";")
	return strings.TrimSpace(strings.ToLower(base))
}

// randomName builds an unguessable filename.
//
// Random rather than derived from the service name or id: the uploads directory
// is served publicly and without an index, so a name that cannot be guessed is
// the only thing keeping an unpublished service's image unlisted. crypto/rand
// rather than a uuid package because go.mod has no uuid dependency and this
// needs no ordering or embedded timestamp.
func randomName(ext string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("upload: generating filename: %w", err)
	}
	return hex.EncodeToString(buf) + ext, nil
}
