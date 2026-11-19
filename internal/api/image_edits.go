package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// imageEditMultipartMemory 是图片编辑表单在内存中保留的字节数，超出部分写入临时文件
const imageEditMultipartMemory = 32 << 20

// imageEditMaskInstruction 是紧跟遮罩图片的说明，白色为编辑区域、黑色为保留区域
const imageEditMaskInstruction = "The previous image is an edit mask for the first image: white pixels mark the area to edit and black pixels mark the area to keep. Apply the following edit only inside the white area and keep everything else unchanged."

// handleOpenAIImageEdits accepts uploaded files or public image URLs.
func (s *server) handleOpenAIImageEdits(w http.ResponseWriter, r *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "application/json" {
		s.handleJSONImageEdits(w, r)
		return
	}
	if err := r.ParseMultipartForm(imageEditMultipartMemory); err != nil {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	request := openAIImageRequest{
		Model: r.FormValue("model"), Prompt: r.FormValue("prompt"), Size: r.FormValue("size"),
		Quality: r.FormValue("quality"), ResponseFormat: r.FormValue("response_format"), OutputFormat: r.FormValue("output_format"),
		SafetyIdentifier: r.FormValue("safety_identifier"), User: r.FormValue("user"),
	}
	request.Stream, _ = strconv.ParseBool(r.FormValue("stream"))
	if value := strings.TrimSpace(r.FormValue("output_compression")); value != "" {
		compression, err := strconv.Atoi(value)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "output_compression must be an integer")
			return
		}
		request.OutputCompression = &compression
	}
	if value := strings.TrimSpace(r.FormValue("n")); value != "" {
		count, err := strconv.Atoi(value)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "n must be an integer")
			return
		}
		request.N = count
	}
	var inputs []aistudio.Part
	for _, header := range append(r.MultipartForm.File["image"], r.MultipartForm.File["image[]"]...) {
		data, err := readMultipartFile(header)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mimeType := declaredMediaType(header.Header.Get("Content-Type"))
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		mimeType, data = normalizeImagePayload(mimeType, data)
		inputs = append(inputs, aistudio.Part{InlineData: &aistudio.Blob{MIME: mimeType, Data: data}})
	}
	for _, field := range []string{"image", "image[]", "image_url", "image_url[]"} {
		for _, reference := range r.MultipartForm.Value[field] {
			part, err := readImageReference(r.Context(), reference)
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			inputs = append(inputs, part)
		}
	}
	if len(inputs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image is required")
		return
	}
	if masks := r.MultipartForm.File["mask"]; len(masks) > 0 {
		data, err := readMultipartFile(masks[0])
		if err == nil {
			data, err = imageEditMask(data)
		}
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		inputs = append(inputs, aistudio.Part{InlineData: &aistudio.Blob{MIME: "image/png", Data: data}}, aistudio.Part{Text: imageEditMaskInstruction})
	} else if reference := strings.TrimSpace(r.FormValue("mask")); reference != "" {
		mask, err := readImageReference(r.Context(), reference)
		if err == nil {
			mask.InlineData.Data, err = imageEditMask(mask.InlineData.Data)
			mask.InlineData.MIME = "image/png"
		}
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		inputs = append(inputs, mask, aistudio.Part{Text: imageEditMaskInstruction})
	}
	s.writeOpenAIImages(w, r, request, inputs, "image_edit")
}

type imageEditJSONRequest struct {
	openAIImageRequest
	Image    json.RawMessage `json:"image"`
	Images   json.RawMessage `json:"images"`
	ImageURL json.RawMessage `json:"image_url"`
	Mask     json.RawMessage `json:"mask"`
}

func (s *server) handleJSONImageEdits(w http.ResponseWriter, r *http.Request) {
	var request imageEditJSONRequest
	if err := decodeJSON(r, &request); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var inputs []aistudio.Part
	for _, raw := range []json.RawMessage{request.Image, request.Images, request.ImageURL} {
		references, err := imageReferences(raw)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		for _, reference := range references {
			part, err := readImageReference(r.Context(), reference)
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			inputs = append(inputs, part)
		}
	}
	if len(inputs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image is required")
		return
	}
	masks, err := imageReferences(request.Mask)
	if err == nil && len(masks) > 1 {
		err = fmt.Errorf("mask must contain exactly one image")
	}
	if err == nil && len(masks) == 1 {
		var part aistudio.Part
		part, err = readImageReference(r.Context(), masks[0])
		if err == nil {
			part.InlineData.Data, err = imageEditMask(part.InlineData.Data)
			part.InlineData.MIME = "image/png"
			inputs = append(inputs, part, aistudio.Part{Text: imageEditMaskInstruction})
		}
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.writeOpenAIImages(w, r, request.openAIImageRequest, inputs, "image_edit")
}

// imageReferences accepts a URL, URL array, or {image_url: "..."} / {url: "..."}.
func imageReferences(raw json.RawMessage) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	readOne := func(value json.RawMessage) (string, error) {
		var text string
		if json.Unmarshal(value, &text) == nil && strings.TrimSpace(text) != "" {
			return text, nil
		}
		var object struct {
			URL      string `json:"url"`
			ImageURL string `json:"image_url"`
		}
		if json.Unmarshal(value, &object) == nil {
			if object.ImageURL != "" {
				return object.ImageURL, nil
			}
			if object.URL != "" {
				return object.URL, nil
			}
		}
		return "", fmt.Errorf("image must be a URL or an object containing image_url or url")
	}
	values := []json.RawMessage{raw}
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("invalid image array")
		}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		reference, err := readOne(value)
		if err != nil {
			return nil, err
		}
		result = append(result, reference)
	}
	return result, nil
}

// readImageReference reuses the public-media downloader, including redirect and private-IP checks.
func readImageReference(ctx context.Context, reference string) (aistudio.Part, error) {
	reference = strings.TrimSpace(reference)
	if !remoteMediaURL(reference) && !strings.HasPrefix(reference, "data:") {
		return aistudio.Part{}, fmt.Errorf("image reference must be an HTTP(S) URL or a data URL")
	}
	part, err := fileOrInlinePart(reference, "")
	if err != nil {
		return aistudio.Part{}, err
	}
	contents := []aistudio.Content{{Parts: []aistudio.Part{part}}}
	if err := inlineRemoteMedia(ctx, contents); err != nil {
		return aistudio.Part{}, err
	}
	part = contents[0].Parts[0]
	if part.InlineData == nil || !strings.HasPrefix(part.InlineData.MIME, "image/") {
		return aistudio.Part{}, fmt.Errorf("image reference did not return an image")
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(part.InlineData.Data)); err != nil {
		return aistudio.Part{}, fmt.Errorf("image reference contains invalid image data")
	}
	return part, nil
}

// readMultipartFile 读取 multipart 文件段的全部内容
func readMultipartFile(header *multipart.FileHeader) ([]byte, error) {
	file, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// responsesImageMask 是 image_generation 工具的编辑遮罩，image_url 为 data URL 或 http(s) URL
type responsesImageMask struct {
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

// imageGenerationMask 读取 input_image_mask 指向的图片并转换为编辑遮罩，file_id 从文件下载
func (s *server) imageGenerationMask(ctx context.Context, mask responsesImageMask) ([]byte, error) {
	if mask.ImageURL == "" {
		service, ok := s.service.(aistudio.FileService)
		if !ok {
			return nil, fmt.Errorf("file content is unavailable")
		}
		media, err := service.DownloadFile(ctx, mask.FileID)
		if err != nil {
			return nil, err
		}
		defer media.Body.Close()
		data, err := io.ReadAll(media.Body)
		if err != nil {
			return nil, err
		}
		return imageEditMask(data)
	}
	part, err := fileOrInlinePart(mask.ImageURL, "")
	if err != nil {
		return nil, err
	}
	contents := []aistudio.Content{{Parts: []aistudio.Part{part}}}
	if err := inlineRemoteMedia(ctx, contents); err != nil {
		return nil, err
	}
	if contents[0].Parts[0].InlineData == nil {
		return nil, fmt.Errorf("image_url must be a data URL or an http(s) URL")
	}
	return imageEditMask(contents[0].Parts[0].InlineData.Data)
}

// withImageMask 把遮罩与说明插入最后一个 user 轮最后一个媒体之后，该轮没有媒体时插在开头
func withImageMask(contents []aistudio.Content, mask []byte) []aistudio.Content {
	masked := []aistudio.Part{{InlineData: &aistudio.Blob{MIME: "image/png", Data: mask}}, {Text: imageEditMaskInstruction}}
	index := len(contents) - 1
	for index >= 0 && contents[index].Role != aistudio.RoleUser {
		index--
	}
	if index < 0 {
		return append(slices.Clone(contents), aistudio.Content{Role: aistudio.RoleUser, Parts: masked})
	}
	position := 0
	for offset, part := range contents[index].Parts {
		if part.InlineData != nil || part.File != nil || part.ExternalMedia != nil {
			position = offset + 1
		}
	}
	edited := slices.Clone(contents)
	edited[index].Parts = slices.Concat(contents[index].Parts[:position], masked, contents[index].Parts[position:])
	return edited
}

// imageEditMask 把 OpenAI 遮罩中完全透明的像素转换为白色、其余像素转换为黑色，返回灰度 PNG
func imageEditMask(data []byte) ([]byte, error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("mask must be a PNG image: %w", err)
	}
	bounds := source.Bounds()
	mask := image.NewGray(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, alpha := source.At(x, y).RGBA(); alpha == 0 {
				mask.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, mask); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}
