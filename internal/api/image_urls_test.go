package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type imageURLService struct {
	aistudio.Service
	picture []byte
	inputs  []aistudio.Part
}

func (service *imageURLService) Models(context.Context) ([]aistudio.Model, error) {
	return []aistudio.Model{{
		ID: "test-image", Methods: []string{"generateContent"},
		Capabilities: map[string]bool{"image_route": true},
	}}, nil
}

func (service *imageURLService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.inputs = request.Contents[0].Parts
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventMedia, Part: aistudio.Part{
		InlineData: &aistudio.Blob{MIME: "image/png", Data: service.picture},
	}}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}
	close(events)
	return events, nil
}

type testImageStorage struct {
	data []byte
	err  error
}

func (storage *testImageStorage) Put(_ context.Context, _ string, data []byte) (string, error) {
	storage.data = data
	return "https://images.example.com/aistudio/test.png", storage.err
}

func pngImage(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestImageEndpointsAlwaysReturnURLs(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, format := range []string{"", "url", "b64_json"} {
			t.Run(format+map[bool]string{true: "-stream", false: "-json"}[stream], func(t *testing.T) {
				storage := &testImageStorage{}
				service := &imageURLService{picture: pngImage(t)}
				handler := NewHandler(service, Config{ImageStorage: storage})
				body, _ := json.Marshal(map[string]any{
					"model": "test-image", "prompt": "draw a tree", "stream": stream, "response_format": format,
				})
				request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				text := response.Body.String()
				if response.Code != http.StatusOK || !strings.Contains(text, `"url":"https://images.example.com/`) ||
					strings.Contains(text, "b64_json") || strings.Contains(text, "data:image/") || len(storage.data) == 0 {
					t.Fatalf("status=%d body=%s", response.Code, text)
				}
			})
		}
	}
}

func TestImagesUseLocalURLsWithoutR2(t *testing.T) {
	service := &imageURLService{picture: pngImage(t)}
	handler := NewHandler(service, Config{MediaDir: t.TempDir(), MediaLimit: 1 << 20})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/images/generations",
		strings.NewReader(`{"model":"test-image","prompt":"tree"}`))
	handler.ServeHTTP(response, request)
	var result struct{ Data []struct{ URL string } }
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Data) != 1 {
		t.Fatalf("body=%s err=%v", response.Body.String(), err)
	}
	imageURL := result.Data[0].URL
	if !strings.HasPrefix(imageURL, "https://api.example.com/media/") {
		t.Fatal(imageURL)
	}
	download := httptest.NewRecorder()
	handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, imageURL, nil))
	if download.Code != http.StatusOK || len(download.Body.Bytes()) == 0 {
		t.Fatalf("image download status=%d", download.Code)
	}
}

func TestImageUploadFailureDoesNotFallBackToBase64(t *testing.T) {
	service := &imageURLService{picture: pngImage(t)}
	handler := NewHandler(service, Config{ImageStorage: &testImageStorage{err: errors.New("R2 upload failed")}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"test-image","prompt":"tree"}`)))
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "b64_json") ||
		strings.Contains(response.Body.String(), `"url"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// URL downloads still resolve a public hostname; only the transport is replaced by an in-memory image response.
func TestImageEditURLsAndUploadedFiles(t *testing.T) {
	picture := pngImage(t)
	oldClient := remoteMediaClient
	remoteMediaClient = &http.Client{Transport: imageRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}},
			Body: io.NopCloser(bytes.NewReader(picture)), Request: request,
		}, nil
	})}
	defer func() { remoteMediaClient = oldClient }()
	for _, body := range []string{
		`{"model":"test-image","prompt":"edit","image":"https://1.1.1.1/reference.png"}`,
		`{"model":"test-image","prompt":"edit","images":[{"image_url":"https://1.1.1.1/a.png"},{"url":"https://1.1.1.1/b.png"}]}`,
		`{"model":"test-image","prompt":"edit","image_url":"https://1.1.1.1/a.png","mask":"https://1.1.1.1/mask.png"}`,
	} {
		service := &imageURLService{picture: picture}
		handler := NewHandler(service, Config{ImageStorage: &testImageStorage{}})
		request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || len(service.inputs) < 2 || service.inputs[0].InlineData == nil {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	for _, uploadFile := range []bool{false, true} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		_ = form.WriteField("model", "test-image")
		_ = form.WriteField("prompt", "edit")
		if uploadFile {
			file, _ := form.CreateFormFile("image[]", "input.png")
			_, _ = file.Write(picture)
		} else {
			_ = form.WriteField("image", "https://1.1.1.1/reference.png")
		}
		_ = form.Close()
		service := &imageURLService{picture: picture}
		handler := NewHandler(service, Config{ImageStorage: &testImageStorage{}})
		request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		request.Header.Set("Content-Type", form.FormDataContentType())
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || service.inputs[0].InlineData == nil {
			t.Fatalf("file=%t status=%d body=%s", uploadFile, response.Code, response.Body.String())
		}
	}
}

type imageRoundTripper func(*http.Request) (*http.Response, error)

func (transport imageRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestImageEditRejectsPrivateURLsAndInvalidImages(t *testing.T) {
	picture := pngImage(t)
	for _, reference := range []string{
		"http://127.0.0.1/private.png", "http://10.0.0.1/private.png",
		"file:///etc/passwd", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("invalid")),
	} {
		service := &imageURLService{picture: picture}
		handler := NewHandler(service, Config{ImageStorage: &testImageStorage{}})
		body, _ := json.Marshal(map[string]any{"model": "test-image", "prompt": "edit", "image": reference})
		request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || len(service.inputs) != 0 {
			t.Fatalf("reference=%s status=%d body=%s", reference, response.Code, response.Body.String())
		}
	}
}
