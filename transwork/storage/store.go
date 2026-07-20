package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	gcsstorage "cloud.google.com/go/storage"
)

// ObjectAttrs is the provider-neutral subset of object metadata the Gressio
// audio flows need.
type ObjectAttrs struct {
	Size        int64
	ContentType string
	Created     time.Time
}

// ObjectStore abstracts the object-storage provider behind the five
// primitives the audio upload/transcription flows use, so a future move off
// GCS (e.g. to S3/R2) is one new implementation plus STORAGE_PROVIDER, with
// no handler changes (DEV-405).
type ObjectStore interface {
	SignedUploadURL(objectKey, contentType string, ttl time.Duration) (string, error)
	SignedDownloadURL(objectKey string, ttl time.Duration) (string, error)
	Attrs(ctx context.Context, objectKey string) (*ObjectAttrs, error)
	Read(ctx context.Context, objectKey string) ([]byte, error)
	ReadRange(ctx context.Context, objectKey string, offset, length int64) ([]byte, error)
	Write(ctx context.Context, objectKey, contentType string, data []byte) error
}

// GetObjectStore returns the configured ObjectStore. STORAGE_PROVIDER selects
// the backend; empty or "gcs" is the GCS store initialized by InitGCSClient.
func GetObjectStore() (ObjectStore, error) {
	provider := strings.TrimSpace(os.Getenv("STORAGE_PROVIDER"))
	switch provider {
	case "", "gcs":
		client, err := GetGCSClient()
		if err != nil {
			return nil, fmt.Errorf("GCS client not initialized: %w", err)
		}
		if bucketName == "" {
			return nil, fmt.Errorf("GCS bucket not configured")
		}
		return &gcsObjectStore{client: client, bucketName: bucketName}, nil
	default:
		return nil, fmt.Errorf("unsupported STORAGE_PROVIDER: %s", provider)
	}
}

type gcsObjectStore struct {
	client     *gcsstorage.Client
	bucketName string
}

func (s *gcsObjectStore) SignedUploadURL(objectKey, contentType string, ttl time.Duration) (string, error) {
	return s.client.Bucket(s.bucketName).SignedURL(objectKey, &gcsstorage.SignedURLOptions{
		Method:      "PUT",
		Expires:     time.Now().Add(ttl),
		ContentType: contentType,
	})
}

func (s *gcsObjectStore) SignedDownloadURL(objectKey string, ttl time.Duration) (string, error) {
	return s.client.Bucket(s.bucketName).SignedURL(objectKey, &gcsstorage.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(ttl),
	})
}

func (s *gcsObjectStore) Attrs(ctx context.Context, objectKey string) (*ObjectAttrs, error) {
	attrs, err := s.client.Bucket(s.bucketName).Object(objectKey).Attrs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read object attrs: %w", err)
	}
	return &ObjectAttrs{
		Size:        attrs.Size,
		ContentType: strings.TrimSpace(attrs.ContentType),
		Created:     attrs.Created,
	}, nil
}

func (s *gcsObjectStore) Read(ctx context.Context, objectKey string) ([]byte, error) {
	reader, err := s.client.Bucket(s.bucketName).Object(objectKey).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create object reader: %w", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read object bytes: %w", err)
	}
	return data, nil
}

func (s *gcsObjectStore) ReadRange(ctx context.Context, objectKey string, offset, length int64) ([]byte, error) {
	reader, err := s.client.Bucket(s.bucketName).Object(objectKey).NewRangeReader(ctx, offset, length)
	if err != nil {
		return nil, fmt.Errorf("failed to create object range reader: %w", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read object range: %w", err)
	}
	return data, nil
}

func (s *gcsObjectStore) Write(ctx context.Context, objectKey, contentType string, data []byte) error {
	writer := s.client.Bucket(s.bucketName).Object(objectKey).NewWriter(ctx)
	writer.ContentType = contentType
	if _, err := writer.Write(data); err != nil {
		writer.Close()
		return fmt.Errorf("failed to write object: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to finalize object write: %w", err)
	}
	return nil
}
