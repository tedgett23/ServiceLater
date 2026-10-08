// Package storagetest runs an in-memory S3 service for tests.
package storagetest

import (
	"net/http/httptest"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/servicelater/servicelater/backend/internal/settings"
)

// Bucket is the bucket StartS3 creates.
const Bucket = "servicelater-test"

// StartS3 starts an in-memory S3 service with an empty bucket, stopped when the test ends, and returns storage
// settings that use it (path-style, signing in with key "test"/"secret", which the fake accepts).
func StartS3(t testing.TB) settings.Storage {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(Bucket); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	s := settings.DefaultStorage
	s.Backend = settings.BackendS3
	s.S3Endpoint = srv.URL
	s.S3Bucket = Bucket
	s.S3PathStyle = true
	s.S3AccessKeyID = "test"
	return s
}
