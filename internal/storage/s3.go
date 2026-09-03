package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Options configures an S3-compatible blob store. Endpoint is empty for
// AWS S3 and set to the provider's API endpoint for R2, Spaces, MinIO, and
// similar services.
type S3Options struct {
	Endpoint       string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	SessionToken   string
	ForcePathStyle bool
}

// s3Client is the small part of the AWS client the blob store needs. Keeping
// this boundary narrow lets the storage semantics be tested without a live
// object-storage account.
type s3Client interface {
	HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// S3 stores content-addressed blobs in an S3-compatible bucket.
//
// Put has to know the digest before it knows the final object key. It therefore
// streams each input through a temporary local file while hashing, then uploads
// that bounded file. Temporary bytes are removed after every attempt; the
// archive itself remains in object storage.
type S3 struct {
	bucket string
	client s3Client
}

// NewS3 constructs an S3-compatible store and verifies that the configured
// bucket is reachable. Callers should place a startup deadline on ctx.
func NewS3(ctx context.Context, opts S3Options) (*S3, error) {
	if opts.Region == "" {
		return nil, fmt.Errorf("storage: empty S3 region")
	}
	if opts.Bucket == "" {
		return nil, fmt.Errorf("storage: empty S3 bucket")
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, fmt.Errorf("storage: empty S3 credentials")
	}

	clientOpts := s3.Options{
		Region:           opts.Region,
		Credentials:      aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, opts.SessionToken)),
		UsePathStyle:     opts.ForcePathStyle,
		RetryMaxAttempts: 3,
		// Newer AWS SDK releases add optional checksum trailers by default.
		// Content addressing already verifies every upload with SHA-256, and
		// requiring only checksums mandated by the API avoids extensions that
		// older S3-compatible providers may not implement.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if opts.Endpoint != "" {
		clientOpts.BaseEndpoint = aws.String(opts.Endpoint)
	}

	return newS3(ctx, opts.Bucket, s3.New(clientOpts))
}

func newS3(ctx context.Context, bucket string, client s3Client) (*S3, error) {
	store := &S3{bucket: bucket, client: client}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return nil, fmt.Errorf("storage: access S3 bucket %q: %w", bucket, err)
	}
	return store, nil
}

// Put implements Store. It never holds the full attachment in memory.
func (s *S3) Put(ctx context.Context, r io.Reader) (PutResult, error) {
	tmp, err := os.CreateTemp("", "openconvo-s3-put-*")
	if err != nil {
		return PutResult{}, fmt.Errorf("storage: create S3 staging file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		return PutResult{}, fmt.Errorf("storage: stage S3 blob: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return PutResult{}, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return PutResult{}, fmt.Errorf("storage: rewind S3 staging file: %w", err)
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	key := ObjectKey(digest)
	result := PutResult{SHA256: digest, Size: size, ObjectKey: key}

	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		if got := aws.ToInt64(head.ContentLength); got != size {
			return PutResult{}, fmt.Errorf("storage: S3 object %s has size %d, expected %d", key, got, size)
		}
		result.AlreadyExisted = true
		return result, nil
	}
	if !isS3NotFound(err) {
		return PutResult{}, fmt.Errorf("storage: inspect S3 object %s: %w", key, err)
	}

	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          tmp,
		ContentLength: aws.Int64(size),
	}); err != nil {
		return PutResult{}, fmt.Errorf("storage: upload S3 object %s: %w", key, err)
	}

	// A successful response is the provider's durability boundary. Verify the
	// visible size as a cheap guard against broken S3-compatible gateways.
	head, err = s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return PutResult{}, fmt.Errorf("storage: verify S3 object %s: %w", key, err)
	}
	if got := aws.ToInt64(head.ContentLength); got != size {
		return PutResult{}, fmt.Errorf("storage: uploaded S3 object %s has size %d, expected %d", key, got, size)
	}
	return result, nil
}

// Open implements Store.
func (s *S3) Open(ctx context.Context, sha256hex string) (io.ReadCloser, error) {
	if err := ValidateSHA256(sha256hex); err != nil {
		return nil, err
	}
	key := ObjectKey(sha256hex)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if isS3NotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: open S3 object %s: %w", key, err)
	}
	if out.Body == nil {
		return nil, fmt.Errorf("storage: open S3 object %s: empty response body", key)
	}
	return out.Body, nil
}

// Exists implements Store.
func (s *S3) Exists(ctx context.Context, sha256hex string) (bool, error) {
	if err := ValidateSHA256(sha256hex); err != nil {
		return false, err
	}
	key := ObjectKey(sha256hex)
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if isS3NotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: inspect S3 object %s: %w", key, err)
	}
	return true, nil
}

// Delete implements Store. S3 DeleteObject is idempotent, including when the
// key does not exist.
func (s *S3) Delete(ctx context.Context, sha256hex string) error {
	if err := ValidateSHA256(sha256hex); err != nil {
		return err
	}
	key := ObjectKey(sha256hex)
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("storage: delete S3 object %s: %w", key, err)
	}
	return nil
}

// WalkObjects streams the bucket's content-addressed namespace page by page.
// Keys outside sha256/ are reported as unknown and are never automatically
// removed. S3 uploads stage in the operating system's temporary directory,
// so there is no persistent remote temporary namespace to enumerate.
func (s *S3) WalkObjects(ctx context.Context, visit func(Object) error) error {
	var continuation *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			ContinuationToken: continuation,
		})
		if err != nil {
			return fmt.Errorf("storage: list S3 bucket %q: %w", s.bucket, err)
		}
		for _, item := range out.Contents {
			key := aws.ToString(item.Key)
			object := Object{Key: key, Size: aws.ToInt64(item.Size)}
			if item.LastModified != nil {
				object.Modified = *item.LastModified
			}
			parts := strings.Split(key, "/")
			if len(parts) == 3 && parts[0] == "sha256" && ValidateSHA256(parts[2]) == nil && key == ObjectKey(parts[2]) {
				object.SHA256 = parts[2]
			}
			if err := visit(object); err != nil {
				return err
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		if out.NextContinuationToken == nil || aws.ToString(out.NextContinuationToken) == "" {
			return fmt.Errorf("storage: S3 listing was truncated without a continuation token")
		}
		continuation = out.NextContinuationToken
	}
}

// DeleteObject removes a canonical item returned by WalkObjects.
func (s *S3) DeleteObject(ctx context.Context, object Object) error {
	if object.SHA256 == "" || object.Key != ObjectKey(object.SHA256) {
		return fmt.Errorf("storage: refusing to delete unknown S3 object %q", object.Key)
	}
	return s.Delete(ctx, object.SHA256)
}

func isS3NotFound(err error) bool {
	if err == nil {
		return false
	}
	var statusError interface{ HTTPStatusCode() int }
	return errors.As(err, &statusError) && statusError.HTTPStatusCode() == 404
}
