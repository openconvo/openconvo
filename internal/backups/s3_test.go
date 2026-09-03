package backups

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type fakeS3Client struct {
	objects map[string][]byte
}

func (f *fakeS3Client) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeS3Client) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	body := f.objects[aws.ToString(in.Key)]
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(body)))}, nil
}

func (f *fakeS3Client) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[aws.ToString(in.Key)] = body
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3Client) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	body := f.objects[aws.ToString(in.Key)]
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: aws.Int64(int64(len(body)))}, nil
}

func (f *fakeS3Client) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func TestS3RepositoryRoundTrip(t *testing.T) {
	client := &fakeS3Client{objects: make(map[string][]byte)}
	repo := &s3Repository{bucket: "backups", client: client}
	ctx := context.Background()
	content := []byte("custom dump")
	if err := repo.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repo.Put(ctx, "db/backup.dump", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	body, size, err := repo.Open(ctx, "db/backup.dump")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if size != int64(len(content)) || !bytes.Equal(got, content) {
		t.Errorf("Open = %d %q", size, got)
	}
	if err := repo.Delete(ctx, "db/backup.dump"); err != nil {
		t.Fatal(err)
	}
	if len(client.objects) != 0 {
		t.Errorf("objects after delete = %v", client.objects)
	}
}
