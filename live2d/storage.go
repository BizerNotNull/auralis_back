package live2d

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	rardecode "github.com/nwaples/rardecode/v2"
)

const (
	maxArchiveBytes  int64 = 200 * 1024 * 1024 // 200 MiB upper guard
	archiveFormatZip       = "zip"
	archiveFormatRar       = "rar"
)

type assetStorage struct {
	baseDir string
}

func newAssetStorageFromEnv() (*assetStorage, error) {
	dir := strings.TrimSpace(os.Getenv("LIVE2D_STORAGE_DIR"))
	if dir == "" {
		dir = "./data/live2d"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("live2d: resolve storage dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("live2d: ensure storage dir: %w", err)
	}
	return &assetStorage{baseDir: abs}, nil
}

func (s *assetStorage) BaseDir() string {
	if s == nil {
		return ""
	}
	return s.baseDir
}

func (s *assetStorage) SaveArchive(fileHeader *multipart.FileHeader, entryHint, previewHint string) (folder string, entryFile string, previewFile *string, err error) {
	if s == nil {
		return "", "", nil, errors.New("live2d: asset storage not configured")
	}
	if fileHeader == nil {
		return "", "", nil, errors.New("live2d: archive file not provided")
	}

	if fileHeader.Size > 0 && fileHeader.Size > maxArchiveBytes {
		return "", "", nil, fmt.Errorf("live2d: archive size exceeds %d bytes", maxArchiveBytes)
	}

	src, err := fileHeader.Open()
	if err != nil {
		return "", "", nil, fmt.Errorf("live2d: open archive: %w", err)
	}
	defer src.Close()

	tmpFile, err := os.CreateTemp("", "live2d-archive-*")
	if err != nil {
		return "", "", nil, fmt.Errorf("live2d: create temp file: %w", err)
	}
	defer func() {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
	}()

	written, err := io.Copy(tmpFile, io.LimitReader(src, maxArchiveBytes+1))
	if err != nil {
		return "", "", nil, fmt.Errorf("live2d: copy archive: %w", err)
	}
	if written > maxArchiveBytes {
		return "", "", nil, fmt.Errorf("live2d: archive size exceeds %d bytes", maxArchiveBytes)
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return "", "", nil, fmt.Errorf("live2d: rewind temp file: %w", err)
	}
	format, err := detectArchiveFormat(tmpFile, fileHeader.Filename)
	if err != nil {
		return "", "", nil, err
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return "", "", nil, fmt.Errorf("live2d: rewind temp file: %w", err)
	}
	stat, err := tmpFile.Stat()
	if err != nil {
		return "", "", nil, fmt.Errorf("live2d: stat temp file: %w", err)
	}

	entryHintNorm := normalizeArchivePath(entryHint)
	previewHintNorm := normalizeArchivePath(previewHint)
	state := newArchiveExtractionState(entryHintNorm, previewHintNorm)

	folder = uuid.NewString()
	destDir := filepath.Join(s.baseDir, folder)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", "", nil, fmt.Errorf("live2d: create model dir: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(destDir)
		}
	}()

	switch format {
	case archiveFormatZip:
		entryFile, previewFile, err = s.extractZip(tmpFile, stat.Size(), destDir, state)
	case archiveFormatRar:
		entryFile, previewFile, err = s.extractRar(tmpFile.Name(), destDir, state)
	default:
		err = fmt.Errorf("live2d: unsupported archive format")
	}
	if err != nil {
		return "", "", nil, err
	}

	cleanup = false
	return folder, entryFile, previewFile, nil
}

func (s *assetStorage) extractZip(tmpFile *os.File, size int64, destDir string, state *archiveExtractionState) (string, *string, error) {
	reader, err := zip.NewReader(tmpFile, size)
	if err != nil {
		return "", nil, fmt.Errorf("live2d: parse archive: %w", err)
	}

	filesExtracted := 0
	for _, file := range reader.File {
		sanitized, err := sanitizeArchiveEntry(file.Name)
		if err != nil {
			return "", nil, err
		}
		if sanitized == "" {
			continue
		}

		relPath := filepath.ToSlash(sanitized)
		targetPath := filepath.Join(destDir, filepath.FromSlash(relPath))

		if !strings.HasPrefix(targetPath, destDir+string(os.PathSeparator)) && targetPath != destDir {
			return "", nil, fmt.Errorf("live2d: archive entry escapes target dir: %s", file.Name)
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return "", nil, fmt.Errorf("live2d: create dir %s: %w", relPath, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return "", nil, fmt.Errorf("live2d: prepare dir %s: %w", relPath, err)
		}

		rc, err := file.Open()
		if err != nil {
			return "", nil, fmt.Errorf("live2d: open entry %s: %w", relPath, err)
		}

		dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return "", nil, fmt.Errorf("live2d: create file %s: %w", relPath, err)
		}

		if _, err := io.Copy(dst, rc); err != nil {
			dst.Close()
			rc.Close()
			return "", nil, fmt.Errorf("live2d: write file %s: %w", relPath, err)
		}

		dst.Close()
		rc.Close()

		filesExtracted++
		state.observe(relPath)
	}

	if filesExtracted == 0 {
		return "", nil, errors.New("live2d: archive is empty")
	}

	return state.resolve()
}

func (s *assetStorage) extractRar(tmpPath string, destDir string, state *archiveExtractionState) (string, *string, error) {
	f, err := os.Open(tmpPath)
	if err != nil {
		return "", nil, fmt.Errorf("live2d: reopen temp archive: %w", err)
	}
	defer f.Close()

	rr, err := rardecode.NewReader(f)
	if err != nil {
		return "", nil, fmt.Errorf("live2d: parse rar archive: %w", err)
	}

	filesExtracted := 0
	for {
		header, err := rr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("live2d: read rar entry: %w", err)
		}

		sanitized, err := sanitizeArchiveEntry(header.Name)
		if err != nil {
			return "", nil, err
		}
		if sanitized == "" {
			if !header.IsDir {
				if _, err := io.Copy(io.Discard, rr); err != nil {
					return "", nil, fmt.Errorf("live2d: discard rar entry: %w", err)
				}
			}
			continue
		}

		relPath := filepath.ToSlash(sanitized)
		targetPath := filepath.Join(destDir, filepath.FromSlash(relPath))

		if !strings.HasPrefix(targetPath, destDir+string(os.PathSeparator)) && targetPath != destDir {
			return "", nil, fmt.Errorf("live2d: archive entry escapes target dir: %s", header.Name)
		}

		if header.IsDir {
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return "", nil, fmt.Errorf("live2d: create dir %s: %w", relPath, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return "", nil, fmt.Errorf("live2d: prepare dir %s: %w", relPath, err)
		}

		dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return "", nil, fmt.Errorf("live2d: create file %s: %w", relPath, err)
		}

		if _, err := io.Copy(dst, rr); err != nil {
			dst.Close()
			return "", nil, fmt.Errorf("live2d: write file %s: %w", relPath, err)
		}

		dst.Close()

		filesExtracted++
		state.observe(relPath)
	}

	if filesExtracted == 0 {
		return "", nil, errors.New("live2d: archive is empty")
	}

	return state.resolve()
}

func (s *assetStorage) Remove(folder string) error {
	if s == nil {
		return nil
	}
	trimmed := strings.TrimSpace(folder)
	if trimmed == "" {
		return nil
	}
	target := filepath.Join(s.baseDir, trimmed)
	if !strings.HasPrefix(target, s.baseDir) {
		return fmt.Errorf("live2d: invalid folder %q", folder)
	}
	return os.RemoveAll(target)
}

func detectArchiveFormat(file *os.File, originalName string) (string, error) {
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(originalName)))
	switch ext {
	case ".zip":
		return archiveFormatZip, nil
	case ".rar":
		return archiveFormatRar, nil
	}

	var header [8]byte
	n, err := file.ReadAt(header[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("live2d: read archive header: %w", err)
	}
	headerSlice := header[:n]

	if len(headerSlice) >= 4 && bytes.Equal(headerSlice[:4], []byte{0x50, 0x4b, 0x03, 0x04}) {
		return archiveFormatZip, nil
	}
	if len(headerSlice) >= 2 && headerSlice[0] == 0x50 && headerSlice[1] == 0x4b {
		return archiveFormatZip, nil
	}
	if len(headerSlice) >= 7 && bytes.Equal(headerSlice[:7], []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x01}) {
		return archiveFormatRar, nil
	}
	if len(headerSlice) >= 6 && bytes.Equal(headerSlice[:6], []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07}) {
		return archiveFormatRar, nil
	}

	if ext != "" {
		return "", fmt.Errorf("live2d: unsupported archive format %q", ext)
	}

	return "", errors.New("live2d: unsupported archive format, only .zip and .rar are accepted")
}

func sanitizeArchiveEntry(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", nil
	}

	normalized := strings.ReplaceAll(trimmed, "\\", "/")
	normalized = path.Clean(normalized)
	normalized = strings.TrimPrefix(normalized, "./")
	if normalized == "." || normalized == "" {
		return "", nil
	}
	if strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("live2d: archive entry %q uses parent traversal", name)
	}
	if strings.HasPrefix(strings.ToLower(normalized), "__macosx/") {
		return "", nil
	}
	return normalized, nil
}

func normalizeArchivePath(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	normalized := strings.ReplaceAll(trimmed, "\\", "/")
	normalized = path.Clean(normalized)
	normalized = strings.TrimPrefix(normalized, "./")
	if normalized == "." {
		return ""
	}
	return normalized
}

func isImagePath(path string) bool {
	switch {
	case strings.HasSuffix(path, ".png"):
		return true
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
		return true
	case strings.HasSuffix(path, ".webp"):
		return true
	case strings.HasSuffix(path, ".gif"):
		return true
	default:
		return false
	}
}

type archiveExtractionState struct {
	entryHint        string
	previewHint      string
	entryHintFound   bool
	previewHintFound bool
	entryCandidate   string
	previewCandidate string
}

func newArchiveExtractionState(entryHint, previewHint string) *archiveExtractionState {
	return &archiveExtractionState{
		entryHint:   entryHint,
		previewHint: previewHint,
	}
}

func (s *archiveExtractionState) observe(relPath string) {
	if relPath == "" {
		return
	}

	if s.entryHint != "" && strings.EqualFold(relPath, s.entryHint) {
		s.entryHintFound = true
	}
	if s.previewHint != "" && strings.EqualFold(relPath, s.previewHint) {
		s.previewHintFound = true
	}

	lower := strings.ToLower(relPath)
	if s.entryCandidate == "" && strings.HasSuffix(lower, ".model3.json") {
		s.entryCandidate = relPath
	}
	if s.previewCandidate == "" && isImagePath(lower) {
		s.previewCandidate = relPath
	}
}

func (s *archiveExtractionState) resolve() (string, *string, error) {
	var entry string
	switch {
	case s.entryHint != "":
		if !s.entryHintFound {
			return "", nil, fmt.Errorf("live2d: entry file %q not found in archive", s.entryHint)
		}
		entry = s.entryHint
	case s.entryCandidate != "":
		entry = s.entryCandidate
	default:
		return "", nil, errors.New("live2d: unable to detect model entry file (.model3.json)")
	}

	var preview string
	switch {
	case s.previewHint != "":
		if !s.previewHintFound {
			return "", nil, fmt.Errorf("live2d: preview file %q not found in archive", s.previewHint)
		}
		preview = s.previewHint
	case s.previewCandidate != "":
		preview = s.previewCandidate
	}

	if preview != "" {
		return entry, &preview, nil
	}
	return entry, nil, nil
}
