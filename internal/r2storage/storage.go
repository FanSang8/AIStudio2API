// Package r2storage uploads generated images to Cloudflare R2.
package r2storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Config struct {
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	PublicBaseURL   string
	KeyPrefix       string
}

func (cfg Config) Enabled() bool {
	return cfg.Endpoint != "" || cfg.Bucket != "" || cfg.AccessKeyID != "" ||
		cfg.SecretAccessKey != "" || cfg.PublicBaseURL != ""
}

func (cfg Config) Validate() error {
	if !cfg.Enabled() {
		return nil
	}
	for _, field := range []struct{ name, value string }{
		{"R2_ENDPOINT", cfg.Endpoint}, {"R2_BUCKET", cfg.Bucket},
		{"R2_ACCESS_KEY_ID", cfg.AccessKeyID}, {"R2_SECRET_ACCESS_KEY", cfg.SecretAccessKey},
		{"R2_PUBLIC_BASE_URL", cfg.PublicBaseURL},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required when R2 is enabled", field.name)
		}
	}
	for _, field := range []struct{ name, value string }{
		{"R2_ENDPOINT", cfg.Endpoint}, {"R2_PUBLIC_BASE_URL", cfg.PublicBaseURL},
	} {
		parsed, err := url.Parse(field.value)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("%s must be an HTTPS URL without credentials, query or fragment", field.name)
		}
		if field.name == "R2_ENDPOINT" && parsed.Path != "" && parsed.Path != "/" {
			return fmt.Errorf("R2_ENDPOINT must not contain a path")
		}
		if field.name == "R2_PUBLIC_BASE_URL" && strings.HasSuffix(parsed.Hostname(), ".r2.cloudflarestorage.com") {
			return fmt.Errorf("R2_PUBLIC_BASE_URL must be the public custom domain, not the S3 endpoint")
		}
	}
	if strings.ContainsAny(cfg.Bucket, "/\\") || strings.TrimSpace(cfg.Bucket) != cfg.Bucket {
		return fmt.Errorf("R2_BUCKET must be a bucket name")
	}
	if strings.ContainsAny(cfg.KeyPrefix, "\\?#") {
		return fmt.Errorf("R2_KEY_PREFIX contains invalid characters")
	}
	for _, segment := range strings.Split(cfg.KeyPrefix, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("R2_KEY_PREFIX must not contain relative path segments")
		}
	}
	return nil
}

type putObjectClient interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type Store struct {
	config Config
	client putObjectClient
}

// New returns nil when no R2 settings have been supplied.
func New(cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled() {
		return nil, nil
	}
	client := s3.New(s3.Options{
		Region: "auto", BaseEndpoint: aws.String(strings.TrimRight(cfg.Endpoint, "/")),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle: true, RetryMaxAttempts: 3,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient:                 &http.Client{Timeout: 90 * time.Second},
	})
	return &Store{config: cfg, client: client}, nil
}

// Put stores the final encoded image under a content hash and returns its public URL.
func (store *Store) Put(ctx context.Context, mimeType string, data []byte) (string, error) {
	extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp"}[mimeType]
	if extension == "" || len(data) == 0 {
		return "", fmt.Errorf("unsupported or empty generated image")
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:]) + extension
	if prefix := strings.Trim(store.config.KeyPrefix, "/"); prefix != "" {
		key = prefix + "/" + key
	}
	_, err := store.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(store.config.Bucket), Key: aws.String(key),
		Body: bytes.NewReader(data), ContentType: aws.String(mimeType),
		ContentLength: aws.Int64(int64(len(data))),
		CacheControl:  aws.String("public, max-age=31536000, immutable"),
	})
	if err != nil {
		// Keep storage credentials, signatures and provider response details out of API errors.
		return "", fmt.Errorf("R2 image upload failed")
	}
	escaped := strings.Split(key, "/")
	for index := range escaped {
		escaped[index] = url.PathEscape(escaped[index])
	}
	return strings.TrimRight(store.config.PublicBaseURL, "/") + "/" + strings.Join(escaped, "/"), nil
}

// Check verifies both authenticated upload and anonymous download through the custom domain.
func (store *Store) Check(ctx context.Context) error {
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aX1sAAAAASUVORK5CYII=")
	imageURL, err := store.Put(ctx, "image/png", data)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return fmt.Errorf("invalid R2 public image URL")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("R2 upload succeeded, but the custom domain could not be reached")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(len(data)+1)))
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		return fmt.Errorf("R2 upload succeeded, but the custom domain did not return the uploaded image (HTTP %d)", response.StatusCode)
	}
	return nil
}
