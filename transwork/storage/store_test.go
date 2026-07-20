package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetObjectStore_UnsupportedProvider(t *testing.T) {
	// A typo'd STORAGE_PROVIDER must fail loudly at request time, not fall
	// back to GCS silently and read the wrong (or no) bucket.
	t.Setenv("STORAGE_PROVIDER", "azure")
	_, err := GetObjectStore()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "azure")
}
