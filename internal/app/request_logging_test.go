package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

type imageLogTestService struct {
	aistudio.Service
	registry *requestRegistry
	picture  []byte
	count    int
	failure  error
	ready    chan struct{}
	mu       sync.Mutex
	ids      []string
}

type imageLogTestAdmin struct {
	api.AdminService
	admin *runtimeAdmin
}

func (admin *imageLogTestAdmin) RecordAccessStart(entry api.AccessLog) {
	admin.admin.RecordAccessStart(entry)
}

func (admin *imageLogTestAdmin) RecordAccessLog(entry api.AccessLog) {
	admin.admin.RecordAccessLog(entry)
}

func (service *imageLogTestService) Models(context.Context) ([]aistudio.Model, error) {
	return []aistudio.Model{{
		ID: "test-image", Methods: []string{"generateContent"},
		Capabilities: map[string]bool{"image_route": true},
	}}, nil
}

func (service *imageLogTestService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	api.SetAccessLogTarget(ctx, request.Model, "")
	api.SetAccessLogGenerationConfig(ctx, request.Config)
	api.SetAccessLogGenerationInput(ctx, request)
	api.StartAccessLog(ctx)
	service.registry.start(ctx, request, func() {})
	service.mu.Lock()
	service.ids = append(service.ids, request.ID)
	if len(service.ids) == service.count {
		close(service.ready)
	}
	service.mu.Unlock()
	select {
	case <-service.ready:
	case <-ctx.Done():
		service.registry.finish(request.ID, "cancelled")
		return nil, ctx.Err()
	}
	service.registry.logRequestProgress(request.ID, "test-account", "INFO", "upstream response")
	if service.failure != nil && strings.HasSuffix(request.ID, "-1") {
		api.SetAccessLogError(ctx, service.failure)
		service.registry.finish(request.ID, "failed")
		return nil, service.failure
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventMedia, Part: aistudio.Part{
		InlineData: &aistudio.Blob{MIME: "image/png", Data: service.picture},
	}}
	usage := &aistudio.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop", Usage: usage}
	close(events)
	api.SetAccessLogGenerationResult(ctx, usage, 0)
	service.registry.finish(request.ID, "completed")
	return events, nil
}

// Multiple images must share one log lifecycle while keeping distinct active request IDs.
func TestImageRequestLogLifecycle(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		path   string
		count  int
		stream bool
		fail   bool
		cancel bool
	}{
		{name: "single", path: "generations", count: 1},
		{name: "two", path: "generations", count: 2},
		{name: "four", path: "generations", count: 4},
		{name: "stream", path: "generations", count: 4, stream: true},
		{name: "edits", path: "edits", count: 4},
		{name: "failure", path: "generations", count: 4, fail: true},
		{name: "cancelled", path: "generations", count: 4, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			registry := newRequestRegistry(ctx)
			service := &imageLogTestService{
				registry: registry, picture: picture.Bytes(), count: test.count, ready: make(chan struct{}),
			}
			if test.fail {
				service.failure = errors.New("upstream generation failed")
			} else if test.cancel {
				service.failure = context.Canceled
			}
			handler := api.NewHandler(service, api.Config{
				Admin:    &imageLogTestAdmin{admin: &runtimeAdmin{requests: registry}},
				MediaDir: t.TempDir(), MediaLimit: 1 << 20,
			})
			payload := map[string]any{"model": "test-image", "prompt": "tree", "n": test.count, "stream": test.stream}
			if test.path == "edits" {
				payload["image"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(picture.Bytes())
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/images/"+test.path, bytes.NewReader(body)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			wantStatus, wantState := http.StatusOK, "completed"
			if test.fail {
				wantStatus, wantState = http.StatusBadGateway, "failed"
			} else if test.cancel {
				wantStatus, wantState = http.StatusServiceUnavailable, "cancelled"
			}
			if response.Code != wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			ids := make(map[string]bool)
			for _, id := range service.ids {
				if ids[id] {
					t.Fatalf("duplicate child ID %q", id)
				}
				ids[id] = true
			}
			if len(ids) != test.count || registry.count() != 0 {
				t.Fatalf("children=%d active=%d", len(ids), registry.count())
			}
			parentID := ""
			starts, finishes, progress := 0, 0, 0
			for _, entry := range registry.logs {
				if entry.Request == nil {
					continue
				}
				if parentID == "" {
					parentID = entry.Request.ID
				}
				if entry.Request.ID != parentID {
					t.Fatalf("orphaned log %s: ID=%q parent=%q", entry.Event, entry.Request.ID, parentID)
				}
				switch entry.Event {
				case "request.started":
					starts++
				case "request.progress":
					progress++
				case "request.finished":
					finishes++
					if entry.Request.State != wantState {
						t.Fatalf("state=%q want=%q", entry.Request.State, wantState)
					}
					if service.failure == nil && (entry.Request.Usage == nil || entry.Request.Usage.TotalTokens != int64(5*test.count)) {
						t.Fatalf("usage=%+v", entry.Request.Usage)
					}
				}
			}
			if starts != 1 || finishes != 1 || progress == 0 || progress > test.count || service.failure == nil && progress != test.count {
				t.Fatalf("starts=%d finishes=%d progress=%d", starts, finishes, progress)
			}
			if !ids[parentID] || strings.HasSuffix(parentID, "-1") || strings.HasSuffix(parentID, "-2") || strings.HasSuffix(parentID, "-3") {
				t.Fatalf("invalid parent ID %q", parentID)
			}
		})
	}
}

func TestRequestProgressAfterFinishIsIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := newRequestRegistry(ctx)
	registry.start(ctx, aistudio.GenerateRequest{ID: "finished"}, func() {})
	registry.finish("finished", "completed")
	registry.logRequestProgress("finished", "test-account", "INFO", "late progress")
	if len(registry.logs) != 0 {
		t.Fatalf("finished request produced running logs: %+v", registry.logs)
	}
}
