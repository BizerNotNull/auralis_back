package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const maxAvatarBytes int64 = 5 * 1024 * 1024

// AvatarStorage provides helpers for storing avatar images in MinIO/S3.
type AvatarStorage struct {
	client    *minio.Client
	bucket    string
	publicURL string
}

// NewAvatarStorageFromEnv initialises AvatarStorage using MINIO_* environment variables.
func NewAvatarStorageFromEnv() (*AvatarStorage, error) {
	endpoint := strings.TrimSpace(os.Getenv("MINIO_ENDPOINT"))
	accessKey := strings.TrimSpace(os.Getenv("MINIO_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("MINIO_SECRET_KEY"))
	bucket := strings.TrimSpace(os.Getenv("MINIO_BUCKET"))
	if endpoint == "" || accessKey == "" || secretKey == "" || bucket == "" {
		return nil, nil
	}

	useSSL := strings.EqualFold(strings.TrimSpace(os.Getenv("MINIO_USE_SSL")), "true")
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("init minio client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("create bucket: %w", err)
		}
	}

	publicURL := strings.TrimSpace(os.Getenv("MINIO_PUBLIC_URL"))
	if publicURL == "" {
		scheme := "http"
		if useSSL {
			scheme = "https"
		}
		publicURL = fmt.Sprintf("%s://%s", scheme, endpoint)
	}

	return &AvatarStorage{
		client:    client,
		bucket:    bucket,
		publicURL: strings.TrimSuffix(publicURL, "/"),
	}, nil
}

// Upload stores the provided avatar image beneath the given path segments.
// The final object key will be avatars/<segments...>/<uuid>.<ext>.
func (s *AvatarStorage) Upload(ctx context.Context, fileHeader *multipart.FileHeader, pathSegments ...string) (string, error) {
	if s == nil || s.client == nil {
		return "", errors.New("avatar storage not configured")
	}
	if fileHeader == nil {
		return "", errors.New("avatar file not provided")
	}

	if fileHeader.Size > 0 && fileHeader.Size > maxAvatarBytes {
		return "", fmt.Errorf("avatar size exceeds %d bytes", maxAvatarBytes)
	}

	src, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("open avatar: %w", err)
	}
	defer src.Close()

	var buffer bytes.Buffer
	limited := io.LimitReader(src, maxAvatarBytes+1)
	written, err := io.Copy(&buffer, limited)
	if err != nil {
		return "", fmt.Errorf("read avatar: %w", err)
	}
	if written > maxAvatarBytes {
		return "", fmt.Errorf("avatar size exceeds %d bytes", maxAvatarBytes)
	}

	data := buffer.Bytes()
	contentType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if !isAllowedAvatarContent(contentType) {
		return "", fmt.Errorf("unsupported avatar content type %q", contentType)
	}

	objectPathSegments := []string{"avatars"}
	for _, segment := range pathSegments {
		trimmed := strings.Trim(segment, "/")
		if trimmed != "" {
			objectPathSegments = append(objectPathSegments, trimmed)
		}
	}
	objectName := path.Join(objectPathSegments...)
	if objectName == "" {
		objectName = "avatars"
	}
	objectName = path.Join(objectName, fmt.Sprintf("%s%s", uuid.NewString(), avatarExtension(fileHeader.Filename, contentType)))

	uploadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	reader := bytes.NewReader(data)
	_, err = s.client.PutObject(uploadCtx, s.bucket, objectName, reader, int64(len(data)), minio.PutObjectOptions{
		ContentType:  contentType,
		CacheControl: "public, max-age=604800",
	})
	if err != nil {
		return "", fmt.Errorf("upload avatar: %w", err)
	}

	return s.buildPublicURL(objectName), nil
}

// Remove deletes the object pointed to by the provided URL/object path.
func (s *AvatarStorage) Remove(ctx context.Context, avatarURL string) error {
	if s == nil || s.client == nil {
		return nil
	}
	objectName, ok := s.objectNameFromURL(avatarURL)
	if !ok {
		return nil
	}

	removeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return s.client.RemoveObject(removeCtx, s.bucket, objectName, minio.RemoveObjectOptions{})
}

// PresignedURL returns a temporary URL for accessing the provided avatar.
func (s *AvatarStorage) PresignedURL(ctx context.Context, raw string, expiry time.Duration) (string, error) {
	if s == nil || s.client == nil {
		return strings.TrimSpace(raw), nil
	}

	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}

	if expiry <= 0 {
		expiry = 15 * time.Minute
	}

	objectName, ok := s.objectNameFromURL(trimmed)
	if !ok {
		if !strings.Contains(trimmed, "://") {
			objectName = strings.TrimPrefix(trimmed, "/")
			objectName = strings.TrimPrefix(objectName, s.bucket+"/")
		}
	}
	if objectName == "" {
		return trimmed, nil
	}

	presignCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url, err := s.client.PresignedGetObject(presignCtx, s.bucket, objectName, expiry, nil)
	if err != nil {
		return "", err
	}

	return url.String(), nil
}

func (s *AvatarStorage) buildPublicURL(objectName string) string {
	base := strings.TrimSuffix(s.publicURL, "/")
	object := strings.TrimPrefix(objectName, "/")
	return fmt.Sprintf("%s/%s/%s", base, s.bucket, object)
}

func (s *AvatarStorage) objectNameFromURL(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}

	base := strings.TrimSuffix(s.publicURL, "/")
	if base != "" && strings.HasPrefix(trimmed, base) {
		candidate := strings.TrimPrefix(trimmed, base)
		candidate = strings.TrimPrefix(candidate, "/")
		candidate = strings.TrimPrefix(candidate, s.bucket+"/")
		candidate = strings.TrimPrefix(candidate, "/")
		if candidate != "" {
			return candidate, true
		}
	}

	target, err := url.Parse(trimmed)
	if err != nil {
		return "", false
	}
	baseURL, err := url.Parse(base)
	if err == nil && baseURL.Host != "" && baseURL.Host == target.Host {
		candidate := strings.TrimPrefix(target.Path, "/")
		candidate = strings.TrimPrefix(candidate, s.bucket+"/")
		candidate = strings.TrimPrefix(candidate, "/")
		if candidate != "" {
			return candidate, true
		}
	}

	if !strings.Contains(trimmed, "://") {
		candidate := strings.TrimPrefix(trimmed, "/")
		candidate = strings.TrimPrefix(candidate, s.bucket+"/")
		candidate = strings.TrimPrefix(candidate, "/")
		if candidate != "" {
			return candidate, true
		}
	}

	return "", false
}

func isAllowedAvatarContent(contentType string) bool {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/png", "image/x-png":
		return true
	case "image/jpeg", "image/pjpeg":
		return true
	case "image/webp":
		return true
	case "image/gif":
		return true
	default:
		return false
	}
}

func avatarExtension(filename, contentType string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/png", "image/x-png":
		return ".png"
	case "image/jpeg", "image/pjpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(filename)))
	if ext == "" {
		return ".bin"
	}
	return ext
}
