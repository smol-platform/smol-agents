// Package agentfs — production S3 driver using aws-sdk-go-v2.
//
// AWSS3 implements the agentfs.S3 interface backed by Amazon S3 (or any
// S3-compatible endpoint). It uses aws-sdk-go-v2/service/s3 for all operations
// and checks versioning once at construction.
//
// Credentials are resolved via the aws-sdk-go-v2 config chain (env vars, EC2
// IMDS, IAM role, etc.). The operator wires broker-resolved credentials into
// the process environment before the worker starts (R-MEM-SEC-1: credentials
// never appear in agent-visible config or logs).
//
// Implements pkg/agentfs.S3.
package agentfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// AWSS3Config holds the settings for an AWSS3 driver instance.
type AWSS3Config struct {
	// Bucket is the S3 bucket name.
	Bucket string

	// Region is the AWS region. Defaults to the SDK default chain when empty.
	Region string

	// Endpoint overrides the S3 endpoint URL for S3-compatible stores
	// (MinIO, LocalStack, etc.). Leave empty for real AWS.
	Endpoint string

	// ForcePathStyle forces path-style S3 URLs ("bucket.s3…" vs "/bucket/…").
	// Required for MinIO and LocalStack.
	ForcePathStyle bool

	// SSEAlgorithm is the server-side encryption algorithm: "" | "AES256" | "aws:kms".
	SSEAlgorithm string

	// KMSKeyARN is the KMS key ARN when SSEAlgorithm is "aws:kms".
	KMSKeyARN string
}

// AWSS3 is a production agentfs.S3 implementation backed by Amazon S3.
type AWSS3 struct {
	cfg    AWSS3Config
	client *s3.Client
}

// NewAWSS3 constructs an AWSS3 driver. It loads AWS credentials from the
// SDK default chain (environment, shared config, EC2 IMDS). Returns an error
// when the bucket name is empty or credential loading fails.
func NewAWSS3(ctx context.Context, cfg AWSS3Config) (*AWSS3, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("agentfs s3: Bucket is required")
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("agentfs s3: load aws config: %w", err)
	}

	s3Opts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}
	if cfg.ForcePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)
	return &AWSS3{cfg: cfg, client: client}, nil
}

// Put uploads an object to S3. Returns the new Version (S3 VersionId + metadata).
// The caller is responsible for providing a seekable reader or byte-buffer body.
func (s *AWSS3) Put(key string, body io.Reader, meta PutMeta) (Version, error) {
	buf, err := io.ReadAll(body)
	if err != nil {
		return Version{}, fmt.Errorf("agentfs s3 put: read body: %w", err)
	}

	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(buf),
		ContentType: aws.String(contentType(meta)),
	}
	if meta.SSEAlgorithm != "" {
		input.ServerSideEncryption = s3types.ServerSideEncryption(meta.SSEAlgorithm)
	} else if s.cfg.SSEAlgorithm != "" {
		input.ServerSideEncryption = s3types.ServerSideEncryption(s.cfg.SSEAlgorithm)
	}
	if meta.KMSKeyARN != "" {
		input.SSEKMSKeyId = aws.String(meta.KMSKeyARN)
	} else if s.cfg.KMSKeyARN != "" {
		input.SSEKMSKeyId = aws.String(s.cfg.KMSKeyARN)
	}
	if len(meta.UserMeta) > 0 {
		input.Metadata = meta.UserMeta
	}

	ctx := context.Background()
	out, err := s.client.PutObject(ctx, input)
	if err != nil {
		return Version{}, fmt.Errorf("agentfs s3 put %q: %w", key, err)
	}

	versionID := ""
	if out.VersionId != nil {
		versionID = *out.VersionId
	}
	return Version{
		ID:        versionID,
		Key:       key,
		CreatedAt: time.Now().UTC(),
		SizeBytes: int64(len(buf)),
	}, nil
}

// ListVersions returns all versions of an object, newest first.
func (s *AWSS3) ListVersions(key string) ([]Version, error) {
	ctx := context.Background()
	out, err := s.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("agentfs s3 list-versions %q: %w", key, err)
	}

	var versions []Version
	for _, v := range out.Versions {
		if v.Key == nil || *v.Key != key {
			continue
		}
		vid := ""
		if v.VersionId != nil {
			vid = *v.VersionId
		}
		size := int64(0)
		if v.Size != nil {
			size = *v.Size
		}
		t := time.Time{}
		if v.LastModified != nil {
			t = *v.LastModified
		}
		versions = append(versions, Version{
			ID:        vid,
			Key:       key,
			CreatedAt: t,
			SizeBytes: size,
		})
	}

	// Sort newest first.
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].CreatedAt.After(versions[j].CreatedAt)
	})
	return versions, nil
}

// Get fetches a specific version (versionID == "" fetches the latest).
func (s *AWSS3) Get(key, versionID string) (io.ReadCloser, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}
	if versionID != "" {
		input.VersionId = aws.String(versionID)
	}

	ctx := context.Background()
	out, err := s.client.GetObject(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("agentfs s3 get %q@%q: %w", key, versionID, err)
	}
	return out.Body, nil
}

// Delete removes one version of an object.
func (s *AWSS3) Delete(key, versionID string) error {
	input := &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}
	if versionID != "" {
		input.VersionId = aws.String(versionID)
	}

	ctx := context.Background()
	_, err := s.client.DeleteObject(ctx, input)
	if err != nil {
		return fmt.Errorf("agentfs s3 delete %q@%q: %w", key, versionID, err)
	}
	return nil
}

// HasVersioning checks whether the bucket has object versioning enabled.
func (s *AWSS3) HasVersioning() (bool, error) {
	ctx := context.Background()
	out, err := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{
		Bucket: aws.String(s.cfg.Bucket),
	})
	if err != nil {
		return false, fmt.Errorf("agentfs s3 get-bucket-versioning: %w", err)
	}
	return out.Status == s3types.BucketVersioningStatusEnabled, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func contentType(meta PutMeta) string {
	if meta.ContentType != "" {
		return meta.ContentType
	}
	return "application/octet-stream"
}

// compile-time assertion: AWSS3 satisfies the S3 interface.
var _ S3 = (*AWSS3)(nil)
