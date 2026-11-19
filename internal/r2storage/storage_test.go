package r2storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func testConfig() Config {
	return Config{
		Endpoint: "https://account.r2.cloudflarestorage.com", Bucket: "images",
		AccessKeyID: "test-access-key", SecretAccessKey: "test-secret",
		PublicBaseURL: "https://images.example.com", KeyPrefix: "aistudio/images",
	}
}

func TestPutUsesS3SigningAndCustomDomain(t *testing.T) {
	data := []byte("encoded-image")
	var uploaded []byte
	var path, authorization, contentType, cacheControl string
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, authorization = r.URL.Path, r.Header.Get("Authorization")
		contentType, cacheControl = r.Header.Get("Content-Type"), r.Header.Get("Cache-Control")
		uploaded, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"test"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()
	cfg := testConfig()
	cfg.Endpoint = endpoint.URL
	store, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store.client = s3.New(store.client.(*s3.Client).Options(), func(options *s3.Options) {
		options.HTTPClient = endpoint.Client()
	})
	publicURL, err := store.Put(context.Background(), "image/png", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential=test-access-key/") ||
		!strings.Contains(authorization, "/auto/s3/aws4_request") {
		t.Fatalf("invalid signing headers: %s", authorization)
	}
	if !bytes.Equal(uploaded, data) || contentType != "image/png" || cacheControl == "" {
		t.Fatalf("upload=%q content-type=%q cache=%q", uploaded, contentType, cacheControl)
	}
	if !strings.HasPrefix(path, "/images/aistudio/images/") ||
		publicURL != "https://images.example.com"+strings.TrimPrefix(path, "/images") ||
		!strings.HasSuffix(publicURL, ".png") {
		t.Fatalf("path=%s public URL=%s", path, publicURL)
	}
}

type rejectedUpload struct{}

func (rejectedUpload) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return nil, fmt.Errorf("provider echoed test-secret and signing information")
}

func TestUploadFailureDoesNotReturnURLOrCredentials(t *testing.T) {
	store := &Store{config: testConfig(), client: rejectedUpload{}}
	imageURL, err := store.Put(context.Background(), "image/png", []byte("image"))
	if err == nil || imageURL != "" || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("url=%q err=%v", imageURL, err)
	}
}

func TestConfigurationRejectsIncompleteOrWrongPublicDomain(t *testing.T) {
	if store, err := New(Config{}); store != nil || err != nil {
		t.Fatalf("disabled R2: store=%v err=%v", store, err)
	}
	for _, mutate := range []func(*Config){
		func(cfg *Config) { cfg.Bucket = "" },
		func(cfg *Config) { cfg.SecretAccessKey = "" },
		func(cfg *Config) { cfg.PublicBaseURL = cfg.Endpoint },
		func(cfg *Config) { cfg.PublicBaseURL += "?token=secret" },
		func(cfg *Config) { cfg.KeyPrefix = "../images" },
	} {
		cfg := testConfig()
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Fatalf("accepted invalid configuration")
		}
	}
}
