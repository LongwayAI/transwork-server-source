package storage

import (
	"context"
	"fmt"
	"os"
	"sync"

	gcsstorage "cloud.google.com/go/storage"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/transwork/brand"
	"google.golang.org/api/option"
)

var (
	client     *gcsstorage.Client
	bucketName string
	initOnce   sync.Once
	initErr    error
)

// InitGCSClient initializes the Google Cloud Storage client used by
// Gressio-specific audio upload and transcription flows.
func InitGCSClient() error {
	initOnce.Do(func() {
		ctx := context.Background()
		credentialsPath := os.Getenv("GCS_CREDENTIALS_PATH")

		var opts []option.ClientOption
		if credentialsPath != "" {
			common.SysLog(fmt.Sprintf("Initializing %s GCS client with credentials from: %s", brand.Name, credentialsPath))
			opts = append(opts, option.WithCredentialsFile(credentialsPath))
		} else {
			common.SysLog(fmt.Sprintf("Initializing %s GCS client with default credentials (ADC)", brand.Name))
		}

		createdClient, err := gcsstorage.NewClient(ctx, opts...)
		if err != nil {
			initErr = fmt.Errorf("failed to create GCS client: %w", err)
			common.SysError(fmt.Sprintf("Failed to initialize %s GCS client: %s", brand.Name, err.Error()))
			return
		}

		client = createdClient
		bucketName = os.Getenv("GCS_BUCKET_NAME")
		if bucketName != "" {
			common.SysLog(fmt.Sprintf("%s GCS initialized with default bucket: %s", brand.Name, bucketName))
		} else {
			common.SysLog(fmt.Sprintf("%s GCS initialized without default bucket", brand.Name))
		}
	})

	return initErr
}

func GetGCSClient() (*gcsstorage.Client, error) {
	if client == nil {
		if err := InitGCSClient(); err != nil {
			return nil, err
		}
	}
	return client, nil
}

func GetGCSBucketName() string {
	return bucketName
}
