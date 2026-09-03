package backups

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type repository interface {
	Check(context.Context) error
	Put(context.Context, string, io.Reader, int64) error
	Open(context.Context, string) (io.ReadCloser, int64, error)
	Delete(context.Context, string) error
}

type repositoryFactory func(Settings) (repository, error)

type backupS3Client interface {
	HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

type s3Repository struct {
	bucket string
	client backupS3Client
}

func newS3Repository(settings Settings, credentialsValue Credentials) (repository, error) {
	if !credentialsValue.Configured() {
		return nil, ErrNotConfigured
	}
	opts := s3.Options{
		Region:                     settings.Region,
		Credentials:                aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(credentialsValue.AccessKey, credentialsValue.SecretKey, credentialsValue.SessionToken)),
		UsePathStyle:               settings.ForcePathStyle,
		RetryMaxAttempts:           3,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if settings.Endpoint != "" {
		opts.BaseEndpoint = aws.String(settings.Endpoint)
	}
	return &s3Repository{bucket: settings.Bucket, client: s3.New(opts)}, nil
}

func (r *s3Repository) Check(ctx context.Context) error {
	if _, err := r.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(r.bucket)}); err != nil {
		return fmt.Errorf("access backup bucket %q: %w", r.bucket, err)
	}
	return nil
}

func (r *s3Repository) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	if _, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(r.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/vnd.postgresql.custom-dump"),
	}); err != nil {
		return fmt.Errorf("upload database backup: %w", err)
	}
	head, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("verify uploaded database backup: %w", err)
	}
	if got := aws.ToInt64(head.ContentLength); got != size {
		return fmt.Errorf("verify uploaded database backup: size is %d, expected %d", got, size)
	}
	return nil
}

func (r *s3Repository) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if isS3NotFound(err) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("download database backup: %w", err)
	}
	if out.Body == nil {
		return nil, 0, fmt.Errorf("download database backup: empty response body")
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

func (r *s3Repository) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("expire database backup: %w", err)
	}
	return nil
}

func isS3NotFound(err error) bool {
	if err == nil {
		return false
	}
	var statusError interface{ HTTPStatusCode() int }
	return errors.As(err, &statusError) && statusError.HTTPStatusCode() == 404
}
