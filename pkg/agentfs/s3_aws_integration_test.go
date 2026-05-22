//go:build integration

// Integration tests for AWSS3. Require a live S3 bucket.
// Skipped when AWS_S3_BUCKET is unset.
//
// Usage:
//
//	AWS_S3_BUCKET="my-test-bucket" AWS_REGION="us-east-1" \
//	  go test -tags integration ./pkg/agentfs/... -run TestAWSS3Integration
package agentfs_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/smol-platform/smol-agents/pkg/agentfs"
)

func TestAWSS3Integration_PutGetDelete(t *testing.T) {
	bucket := os.Getenv("AWS_S3_BUCKET")
	if bucket == "" {
		t.Skip("AWS_S3_BUCKET not set; skipping S3 integration tests")
	}
	region := os.Getenv("AWS_REGION")

	ctx := context.Background()
	s3, err := agentfs.NewAWSS3(ctx, agentfs.AWSS3Config{
		Bucket: bucket,
		Region: region,
	})
	if err != nil {
		t.Fatalf("NewAWSS3: %v", err)
	}

	key := "integ-test/" + time.Now().Format("20060102-150405.999")
	content := []byte("s3 integration test content")

	v, err := s3.Put(key, bytes.NewReader(content), agentfs.PutMeta{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Logf("Put version: %s", v.ID)

	rc, err := s3.Get(key, "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}

	versions, err := s3.ListVersions(key)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("expected at least one version after Put")
	}

	if v.ID != "" {
		if err := s3.Delete(key, v.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
}

func TestAWSS3Integration_HasVersioning(t *testing.T) {
	bucket := os.Getenv("AWS_S3_BUCKET")
	if bucket == "" {
		t.Skip("AWS_S3_BUCKET not set")
	}
	ctx := context.Background()
	s3, err := agentfs.NewAWSS3(ctx, agentfs.AWSS3Config{Bucket: bucket})
	if err != nil {
		t.Fatalf("NewAWSS3: %v", err)
	}
	ok, err := s3.HasVersioning()
	if err != nil {
		t.Fatalf("HasVersioning: %v", err)
	}
	t.Logf("bucket versioning enabled: %v", ok)
}
