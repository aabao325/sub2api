package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type GrokMediaEndpoint string

const (
	GrokMediaEndpointImagesGenerations GrokMediaEndpoint = "images_generations"
	GrokMediaEndpointImagesEdits       GrokMediaEndpoint = "images_edits"
	GrokMediaEndpointVideosGenerations GrokMediaEndpoint = "videos_generations"
	GrokMediaEndpointVideosEdits       GrokMediaEndpoint = "videos_edits"
	GrokMediaEndpointVideosExtensions  GrokMediaEndpoint = "videos_extensions"
	GrokMediaEndpointVideoStatus       GrokMediaEndpoint = "video_status"
	// GrokMediaEndpointVideoContent 对应 new-api / OpenAI Sora 视频协议里的
	// GET /v1/videos/{id}/content：上游 xAI 并没有独立的 content 端点，视频地址
	// 内嵌在状态查询响应的 video.url 字段里，因此该端点需要先查询状态拿到
	// video.url，再代理下载真正的视频二进制内容返回给调用方。
	GrokMediaEndpointVideoContent GrokMediaEndpoint = "video_content"
)

func (e GrokMediaEndpoint) RequiresRequestBody() bool {
	switch e {
	case GrokMediaEndpointVideoStatus, GrokMediaEndpointVideoContent:
		return false
	default:
		return true
	}
}

func (e GrokMediaEndpoint) IsGenerationRequest() bool {
	switch e {
	case GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits, GrokMediaEndpointVideosGenerations, GrokMediaEndpointVideosEdits, GrokMediaEndpointVideosExtensions:
		return true
	default:
		return false
	}
}

type GrokMediaRequestInfo struct {
	Model           string
	Prompt          string
	N               int
	Size            string
	SizeTier        string
	Resolution      string
	DurationSeconds int
	InputImageURLs  []string
	MaskImageURL    string
	Uploads         []OpenAIImagesUpload
	MaskUpload      *OpenAIImagesUpload
	// ResponseFormat 对应图片接口的 response_format 参数（"url" 或 "b64_json"）。
	// 客户端未显式传入时为空字符串，由 applyGrokMediaImageResponseFormatDefault
	// 补默认值（b64_json），而不是在这里直接写死默认值——这样才能区分"用户没传"
	// 和"用户显式传了某个值"两种情况。
	ResponseFormat string
}

func (r GrokMediaRequestInfo) ModerationBody() []byte {
	payload := map[string]any{}
	if prompt := strings.TrimSpace(r.Prompt); prompt != "" {
		payload["prompt"] = prompt
	}

	images := make([]map[string]string, 0, len(r.InputImageURLs)+len(r.Uploads)+1)
	for _, imageURL := range r.InputImageURLs {
		if imageURL = strings.TrimSpace(imageURL); imageURL != "" {
			images = append(images, map[string]string{"image_url": imageURL})
		}
	}
	for _, upload := range r.Uploads {
		if dataURL := upload.ModerationDataURL(); dataURL != "" {
			images = append(images, map[string]string{"image_url": dataURL})
		}
	}
	if maskURL := strings.TrimSpace(r.MaskImageURL); maskURL != "" {
		images = append(images, map[string]string{"image_url": maskURL})
	}
	if r.MaskUpload != nil {
		if dataURL := r.MaskUpload.ModerationDataURL(); dataURL != "" {
			images = append(images, map[string]string{"image_url": dataURL})
		}
	}
	if len(images) > 0 {
		payload["images"] = images
	}
	if len(payload) == 0 {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return body
}

func (e GrokMediaEndpoint) httpMethod() string {
	switch e {
	case GrokMediaEndpointVideoStatus, GrokMediaEndpointVideoContent:
		return http.MethodGet
	default:
		return http.MethodPost
	}
}

func ExtractGrokMediaModel(contentType string, body []byte) string {
	return ParseGrokMediaRequest(contentType, body).Model
}

func ParseGrokMediaRequest(contentType string, body []byte) GrokMediaRequestInfo {
	info := GrokMediaRequestInfo{N: 1}
	if gjson.ValidBytes(body) {
		parseGrokMediaJSONRequest(body, &info)
	} else {
		parseGrokMediaMultipartRequest(contentType, body, &info)
	}
	info.Model = strings.TrimSpace(info.Model)
	info.Prompt = strings.TrimSpace(info.Prompt)
	info.Size = strings.TrimSpace(info.Size)
	info.SizeTier = NormalizeImageBillingTierOrDefault(info.Size)
	info.Resolution = NormalizeVideoBillingResolutionOrDefault(info.Resolution)
	info.DurationSeconds = NormalizeVideoBillingDurationSecondsOrDefault(info.DurationSeconds)
	if info.N <= 0 {
		info.N = 1
	}
	return info
}

func parseGrokMediaJSONRequest(body []byte, info *GrokMediaRequestInfo) {
	if info == nil {
		return
	}
	info.Model = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	info.Prompt = strings.TrimSpace(gjson.GetBytes(body, "prompt").String())
	info.Size = strings.TrimSpace(gjson.GetBytes(body, "size").String())
	info.Resolution = strings.TrimSpace(gjson.GetBytes(body, "resolution").String())
	if duration := gjson.GetBytes(body, "duration"); duration.Exists() && duration.Type == gjson.Number {
		info.DurationSeconds = int(duration.Int())
	}
	// OpenAI Sora / new-api 风格的视频请求用字符串 "seconds" 字段表示时长
	// （而不是数字型 "duration"），这里作为等价别名解析，未显式提供 duration 时使用。
	if info.DurationSeconds <= 0 {
		if seconds := gjson.GetBytes(body, "seconds"); seconds.Exists() {
			raw := strings.TrimSpace(seconds.String())
			if parsed, err := strconv.Atoi(raw); err == nil {
				info.DurationSeconds = parsed
			}
		}
	}
	if n := gjson.GetBytes(body, "n"); n.Exists() && n.Type == gjson.Number {
		info.N = int(n.Int())
	}
	info.ResponseFormat = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "response_format").String()))
	appendJSONImageURLs := func(value gjson.Result) {
		if !value.Exists() {
			return
		}
		switch {
		case value.IsArray():
			for _, item := range value.Array() {
				if imageURL := strings.TrimSpace(item.Get("url").String()); imageURL != "" {
					info.InputImageURLs = append(info.InputImageURLs, imageURL)
					continue
				}
				if imageURL := strings.TrimSpace(item.Get("image_url").String()); imageURL != "" {
					info.InputImageURLs = append(info.InputImageURLs, imageURL)
					continue
				}
				if item.Type == gjson.String {
					imageURL := strings.TrimSpace(item.String())
					if imageURL == "" {
						continue
					}
					info.InputImageURLs = append(info.InputImageURLs, imageURL)
				}
			}
		default:
			if imageURL := strings.TrimSpace(value.Get("url").String()); imageURL != "" {
				info.InputImageURLs = append(info.InputImageURLs, imageURL)
				return
			}
			if imageURL := strings.TrimSpace(value.Get("image_url").String()); imageURL != "" {
				info.InputImageURLs = append(info.InputImageURLs, imageURL)
				return
			}
			if value.Type == gjson.String {
				imageURL := strings.TrimSpace(value.String())
				if imageURL == "" {
					return
				}
				info.InputImageURLs = append(info.InputImageURLs, imageURL)
			}
		}
	}
	appendJSONImageURLs(gjson.GetBytes(body, "image"))
	appendJSONImageURLs(gjson.GetBytes(body, "images"))
	// xAI 官方的图生视频参考图数组字段（多图 reference-to-video）。
	appendJSONImageURLs(gjson.GetBytes(body, "reference_images"))
	// OpenAI Sora / new-api 风格的图生视频请求用 "input_reference" 传参考图,
	// 效果等价于 image/images,这里合并解析。
	appendJSONImageURLs(gjson.GetBytes(body, "input_reference"))
	info.MaskImageURL = strings.TrimSpace(gjson.GetBytes(body, "mask.image_url").String())
}

func parseGrokMediaMultipartRequest(contentType string, body []byte, info *GrokMediaRequestInfo) {
	if info == nil {
		return
	}
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
		name := strings.TrimSpace(part.FormName())
		if name == "" {
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, openAIImageMaxUploadPartSize))
		_ = part.Close()
		if err != nil {
			return
		}
		fileName := strings.TrimSpace(part.FileName())
		partContentType := strings.TrimSpace(part.Header.Get("Content-Type"))
		if fileName != "" {
			upload := OpenAIImagesUpload{
				FieldName:   name,
				FileName:    fileName,
				ContentType: partContentType,
				Data:        data,
			}
			if name == "mask" {
				info.MaskUpload = &upload
				continue
			}
			if name == "image" || strings.HasPrefix(name, "image[") {
				info.Uploads = append(info.Uploads, upload)
			}
			continue
		}

		value := strings.TrimSpace(string(data))
		switch name {
		case "model":
			info.Model = value
		case "prompt":
			info.Prompt = value
		case "size":
			info.Size = value
		case "resolution":
			info.Resolution = value
		case "duration":
			if duration, err := strconv.Atoi(value); err == nil {
				info.DurationSeconds = duration
			}
		case "seconds":
			if info.DurationSeconds <= 0 {
				if duration, err := strconv.Atoi(value); err == nil {
					info.DurationSeconds = duration
				}
			}
		case "n":
			if n, err := strconv.Atoi(value); err == nil {
				info.N = n
			}
		case "response_format":
			info.ResponseFormat = strings.ToLower(value)
		case "image", "image_url", "input_reference":
			if value != "" {
				info.InputImageURLs = append(info.InputImageURLs, value)
			}
		case "mask", "mask_image_url":
			info.MaskImageURL = value
		}
	}
}

func GrokMediaVideoRequestSessionHash(requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ""
	}
	return "grok-video:" + DeriveSessionHashFromSeed(requestID)
}

func (s *OpenAIGatewayService) BindGrokMediaVideoRequestAccount(ctx context.Context, groupID *int64, requestID string, accountID int64) error {
	return s.BindStickySession(ctx, groupID, GrokMediaVideoRequestSessionHash(requestID), accountID)
}

func (e GrokMediaEndpoint) upstreamURL(baseURL, requestID string) (string, error) {
	switch e {
	case GrokMediaEndpointImagesGenerations:
		return xai.BuildImagesGenerationsURL(baseURL)
	case GrokMediaEndpointImagesEdits:
		return xai.BuildImagesEditsURL(baseURL)
	case GrokMediaEndpointVideosGenerations:
		return xai.BuildVideosGenerationsURL(baseURL)
<<<<<<< Updated upstream
	case GrokMediaEndpointVideosEdits:
		return xai.BuildVideosEditsURL(baseURL)
	case GrokMediaEndpointVideosExtensions:
		return xai.BuildVideosExtensionsURL(baseURL)
	case GrokMediaEndpointVideoStatus:
=======
	case GrokMediaEndpointVideoStatus, GrokMediaEndpointVideoContent:
>>>>>>> Stashed changes
		return xai.BuildVideoURL(baseURL, requestID)
	default:
		return "", fmt.Errorf("unsupported grok media endpoint: %s", e)
	}
}

func (s *OpenAIGatewayService) ForwardGrokMedia(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	endpoint GrokMediaEndpoint,
	requestID string,
	body []byte,
	contentType string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	if account == nil {
		return nil, fmt.Errorf("grok account is required")
	}
	if account.Platform != PlatformGrok {
		return nil, fmt.Errorf("account platform %s is not supported for grok media", account.Platform)
	}

	// GET /v1/videos/{id}/content 在 xAI 侧没有对应端点：视频地址嵌在状态查询
	// 响应的 video.url 里，需要先查状态再代理下载真正的二进制内容，走独立流程。
	if endpoint == GrokMediaEndpointVideoContent {
		return s.forwardGrokVideoContent(ctx, c, account, requestID, startTime)
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	targetURL, err := endpoint.upstreamURL(account.GetGrokMediaBaseURL(), requestID)
	if err != nil {
		return nil, err
	}

	body, contentType, err = prepareGrokMediaForwardBody(endpoint, body, contentType)
	if err != nil {
		return nil, err
	}
	body, contentType, err = normalizeGrokMediaImageInputs(endpoint, body, contentType)
	if err != nil {
		return nil, err
	}
	body, contentType, err = normalizeGrokMediaForwardBody(endpoint, body, contentType)
	if err != nil {
		return nil, err
	}
	requestInfo := ParseGrokMediaRequest(contentType, body)
	body, contentType, err = sanitizeGrokMediaForwardBody(endpoint, body, contentType)
	if err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if endpoint.RequiresRequestBody() {
		bodyReader = bytes.NewReader(body)
	}
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, endpoint.httpMethod(), targetURL, bodyReader)
	if err != nil {
		return nil, err
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+token)
	upstreamReq.Header.Set("Accept", "application/json")
	applyGrokCLIHeaders(upstreamReq.Header)
	if endpoint.RequiresRequestBody() {
		contentType = strings.TrimSpace(contentType)
		if contentType == "" {
			contentType = "application/json"
		}
		upstreamReq.Header.Set("Content-Type", contentType)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	upstreamStart := time.Now()
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	requestIDHeader := firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id"))
	requestModel := requestInfo.Model
	if resp.StatusCode >= 400 {
		return s.handleGrokMediaErrorResponse(ctx, resp, c, account, requestIDHeader, requestModel)
	}

	s.updateGrokUsageSnapshot(ctx, account, xai.ParseQuotaHeaders(resp.Header, resp.StatusCode))
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	// 计费/用量提取必须基于 xAI 原始响应体（字段名如 request_id 等），
	// 因此要在把响应体转换成 OpenAI Sora 标准格式之前完成。
	usage := grokMediaUsageFromResponse(endpoint, requestInfo, respBody)

	outBody := respBody
	switch endpoint {
	case GrokMediaEndpointVideosGenerations:
		// new-api 等中转程序把 sub2api 当作 "Sora 渠道" 上游时，要求生成接口返回
		// OpenAI Sora 标准的 video 对象（id/object/status/progress/...），而不是
		// xAI 原生的 {"request_id": "..."} 格式。
		if usage.ResponseID != "" {
			outBody = buildGrokVideoSoraGenerationResponse(usage.ResponseID, requestModel, requestInfo)
		}
	case GrokMediaEndpointVideoStatus:
		outBody = buildGrokVideoSoraStatusResponse(requestID, respBody)
	}
	writeGrokMediaResponse(c, resp, outBody, s.responseHeaderFilter)
	return &OpenAIForwardResult{
		RequestID:            requestIDHeader,
		ResponseID:           usage.ResponseID,
		Usage:                usage.Usage,
		Model:                requestModel,
		BillingModel:         requestModel,
		UpstreamModel:        requestModel,
		ResponseHeaders:      resp.Header.Clone(),
		Duration:             time.Since(startTime),
		ImageCount:           usage.ImageCount,
		ImageSize:            usage.ImageSize,
		ImageInputSize:       usage.ImageInputSize,
		ImageOutputSizes:     usage.ImageOutputSizes,
		VideoCount:           usage.VideoCount,
		VideoResolution:      usage.VideoResolution,
		VideoDurationSeconds: usage.VideoDurationSeconds,
	}, nil
}

func prepareGrokMediaForwardBody(endpoint GrokMediaEndpoint, body []byte, contentType string) ([]byte, string, error) {
	if endpoint != GrokMediaEndpointImagesEdits || gjson.ValidBytes(body) {
		return body, contentType, nil
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return body, contentType, nil
	}

	info := ParseGrokMediaRequest(contentType, body)
	payload := make(map[string]any)
	if info.Model != "" {
		payload["model"] = info.Model
	}
	if info.Prompt != "" {
		payload["prompt"] = info.Prompt
	}
	if info.N > 1 {
		payload["n"] = info.N
	}
	if info.Size != "" {
		payload["size"] = info.Size
	}
	if info.ResponseFormat != "" {
		payload["response_format"] = info.ResponseFormat
	}

	images := make([]map[string]string, 0, len(info.InputImageURLs)+len(info.Uploads))
	for _, imageURL := range info.InputImageURLs {
		if imageURL = strings.TrimSpace(imageURL); imageURL != "" {
			images = append(images, map[string]string{"url": imageURL})
		}
	}
	for _, upload := range info.Uploads {
		dataURL, err := openAIImageUploadToDataURL(upload)
		if err != nil {
			return nil, "", err
		}
		images = append(images, map[string]string{"url": dataURL})
	}
	// xAI 的 image/images 是互斥字段：单图用 "image"（对象），多图用 "images"
	// （数组），两者不能同时出现，否则上游返回 400。
	if len(images) == 1 {
		payload["image"] = images[0]
	} else if len(images) > 1 {
		payload["images"] = images
	}

	maskImageURL := strings.TrimSpace(info.MaskImageURL)
	if info.MaskUpload != nil {
		dataURL, err := openAIImageUploadToDataURL(*info.MaskUpload)
		if err != nil {
			return nil, "", err
		}
		maskImageURL = dataURL
	}
	if maskImageURL != "" {
		payload["mask"] = map[string]string{"image_url": maskImageURL}
	}

	out, err := marshalOpenAIUpstreamJSON(payload)
	if err != nil {
		return nil, "", err
	}
	return out, "application/json", nil
}

func normalizeGrokMediaForwardBody(endpoint GrokMediaEndpoint, body []byte, contentType string) ([]byte, string, error) {
	if !endpoint.RequiresRequestBody() || !gjson.ValidBytes(body) {
		return body, contentType, nil
	}
	info := ParseGrokMediaRequest(contentType, body)
	// normalizeGrokMediaImageInputs 已经在此之前把图片字段规整成互斥的
	// "image"（单图）/"reference_images"（多图）二选一，这里直接检查 body 上
	// 实际落地的字段名，才能准确区分 image-to-video 与 reference-to-video 两种
	// 模式——info.HasInputImage() 对两者都返回 true，不足以区分。
	hasReferenceImages := gjson.GetBytes(body, "reference_images").Exists()
	upstreamModel := normalizeGrokMediaModelForEndpoint(endpoint, info.Model, info.HasInputImage(), hasReferenceImages)
	if upstreamModel == "" || upstreamModel == info.Model {
		return body, contentType, nil
	}
	out, err := sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, "", fmt.Errorf("rewrite grok media model: %w", err)
	}
	return out, contentType, nil
}

// normalizeGrokMediaImageInputs 把请求体里的图片引用字段规整成 xAI 官方要求的格式。
//
// new-api 等中转程序把 sub2api 配置为上游渠道时，其自身的任务提交结构体里
// image/images/input_reference 字段固定是字符串/字符串数组类型（无法承载 xAI
// 要求的 {"url": "..."} 对象），因此这里统一识别并转换：
//   - 字符串 → {"url": "<字符串>"}
//   - 已经是对象的（url/image_url/file_id）→ 保留其中的 url/file_id，键名统一成 "url"
//
// 同时按 xAI 的互斥约束重新归位到正确的字段名：
//   - images/edits：单图用 "image"（对象），多图用 "images"（数组）
//   - videos/generations：单图用 "image"（图生视频 I2V），多图用 "reference_images"
//     （参考图生视频 R2V，对应 xAI 官方 reference_images 数组格式）
//
// 输入可能同时携带 image/images/reference_images/input_reference 中的多个字段
// （不同版本的中转程序命名不一致），这里全部合并去重后按数量重新分配到唯一的
// 目标字段，避免把互斥字段同时发给上游导致 400。
func normalizeGrokMediaImageInputs(endpoint GrokMediaEndpoint, body []byte, contentType string) ([]byte, string, error) {
	if !gjson.ValidBytes(body) {
		return body, contentType, nil
	}
	if endpoint != GrokMediaEndpointImagesEdits && endpoint != GrokMediaEndpointVideosGenerations {
		return body, contentType, nil
	}

	sourceKeys := []string{"image", "images", "reference_images", "input_reference"}
	hasAny := false
	for _, key := range sourceKeys {
		if gjson.GetBytes(body, key).Exists() {
			hasAny = true
			break
		}
	}
	if !hasAny {
		return body, contentType, nil
	}

	var refs []map[string]string
	appendRef := func(value gjson.Result) {
		if !value.Exists() {
			return
		}
		if value.IsArray() {
			for _, item := range value.Array() {
				if ref := grokImageRefFromJSON(item); ref != nil {
					refs = append(refs, ref)
				}
			}
			return
		}
		if ref := grokImageRefFromJSON(value); ref != nil {
			refs = append(refs, ref)
		}
	}
	for _, key := range sourceKeys {
		appendRef(gjson.GetBytes(body, key))
	}

	out := body
	var err error
	for _, key := range sourceKeys {
		if gjson.GetBytes(out, key).Exists() {
			out, err = sjson.DeleteBytes(out, key)
			if err != nil {
				return nil, "", fmt.Errorf("normalize grok media image fields: %w", err)
			}
		}
	}

	if len(refs) == 0 {
		return out, contentType, nil
	}

	pluralKey := "images"
	if endpoint == GrokMediaEndpointVideosGenerations {
		pluralKey = "reference_images"
	}

	if len(refs) == 1 {
		out, err = sjson.SetBytes(out, "image", refs[0])
	} else {
		out, err = sjson.SetBytes(out, pluralKey, refs)
	}
	if err != nil {
		return nil, "", fmt.Errorf("normalize grok media image fields: %w", err)
	}
	return out, contentType, nil
}

// grokImageRefFromJSON 把单个图片引用（字符串 URL，或已经是 {"url"/"image_url": "...",
// "file_id": "..."} 形式的对象）规整成 xAI 官方要求的对象格式，统一使用 "url" 键名。
func grokImageRefFromJSON(value gjson.Result) map[string]string {
	switch {
	case value.Type == gjson.String:
		url := strings.TrimSpace(value.String())
		if url == "" {
			return nil
		}
		return map[string]string{"url": url}
	case value.IsObject():
		ref := map[string]string{}
		if url := strings.TrimSpace(value.Get("url").String()); url != "" {
			ref["url"] = url
		} else if url := strings.TrimSpace(value.Get("image_url").String()); url != "" {
			ref["url"] = url
		}
		if fileID := strings.TrimSpace(value.Get("file_id").String()); fileID != "" {
			ref["file_id"] = fileID
		}
		if len(ref) == 0 {
			return nil
		}
		return ref
	default:
		return nil
	}
}

func sanitizeGrokMediaForwardBody(endpoint GrokMediaEndpoint, body []byte, contentType string) ([]byte, string, error) {
	if !endpoint.RequiresRequestBody() || !gjson.ValidBytes(body) {
		return body, contentType, nil
	}
	switch endpoint {
	case GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits:
		out := body
		if gjson.GetBytes(out, "size").Exists() {
			var err error
			out, err = sjson.DeleteBytes(out, "size")
			if err != nil {
				return nil, "", fmt.Errorf("sanitize grok media size: %w", err)
			}
		}
		out, err := applyGrokMediaImageResponseFormatDefault(out)
		if err != nil {
			return nil, "", err
		}
		return out, contentType, nil
	default:
		return body, contentType, nil
	}
}

// applyGrokMediaImageResponseFormatDefault 补齐图片接口的 response_format 默认值。
//
// xAI 的 /v1/images/generations、/v1/images/edits 与标准 OpenAI 图片接口一样原生
// 支持 response_format=url|b64_json，且上游返回体本身就是标准 OpenAI 格式
// （data: [{url:...}] 或 data: [{b64_json:...}]），因此这里不需要额外转换响应体，
// 只需要在客户端没有显式传 response_format 时，把默认值改成 b64_json（而不是
// xAI/OpenAI 原生默认的 "url"），并原样透传客户端显式指定的取值，让调用方可以
// 自主选择要 url 还是 b64_json。
func applyGrokMediaImageResponseFormatDefault(body []byte) ([]byte, error) {
	if gjson.GetBytes(body, "response_format").Exists() {
		return body, nil
	}
	out, err := sjson.SetBytes(body, "response_format", "b64_json")
	if err != nil {
		return nil, fmt.Errorf("set grok media response_format default: %w", err)
	}
	return out, nil
}

func (r GrokMediaRequestInfo) HasInputImage() bool {
	return len(r.InputImageURLs) > 0 || len(r.Uploads) > 0
}

func normalizeGrokMediaModelForEndpoint(endpoint GrokMediaEndpoint, model string, hasInputImage bool, hasReferenceImages bool) string {
	model = strings.TrimSpace(model)
	switch endpoint {
	case GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits:
		if model == "grok-imagine" {
			return "grok-imagine-image-quality"
		}
	case GrokMediaEndpointVideosGenerations:
		// 兼容通过 new-api 等中转程序以 "Sora 渠道" 转发过来的请求：这类请求携带的
		// 是 OpenAI Sora 的模型名（sora-2/sora-2-pro），直接映射到对应的 Grok 视频模型，
		// 管理员无需在中转程序里额外配置模型重定向。
		switch model {
		case "sora-2":
			model = "grok-imagine-video"
		case "sora-2-pro":
			model = "grok-imagine-video-1.5"
		}
		// xAI 官方明确 grok-imagine-video-1.5 不支持 reference-to-video（多参考图）
		// 模式，只有 grok-imagine-video 支持；无论请求方指定的是哪个模型，只要带
		// 了 reference_images 就必须强制降级，否则上游会返回
		// "`reference_images` is not supported for this model." 的 400 错误。
		if hasReferenceImages {
			return "grok-imagine-video"
		}
		if model == "grok-imagine-video-1.5" && !hasInputImage {
			return "grok-imagine-video"
		}
	}
	return model
}

type grokMediaUsageMetadata struct {
	ResponseID           string
	Usage                OpenAIUsage
	ImageCount           int
	ImageSize            string
	ImageInputSize       string
	ImageOutputSizes     []string
	VideoCount           int
	VideoResolution      string
	VideoDurationSeconds int
}

func grokMediaUsageFromResponse(endpoint GrokMediaEndpoint, requestInfo GrokMediaRequestInfo, responseBody []byte) grokMediaUsageMetadata {
	usage, _ := extractOpenAIUsageFromJSONBytes(responseBody)
	meta := grokMediaUsageMetadata{Usage: usage}
	switch endpoint {
	case GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits:
		imageCount := countOpenAIResponseImageOutputsFromJSONBytes(responseBody)
		if imageCount <= 0 {
			imageCount = requestInfo.N
		}
		if imageCount <= 0 {
			imageCount = 1
		}
		meta.ImageCount = imageCount
		meta.ImageSize = requestInfo.SizeTier
		meta.ImageInputSize = requestInfo.Size
		meta.ImageOutputSizes = collectOpenAIResponseImageOutputSizesFromJSONBytes(responseBody)
	case GrokMediaEndpointVideosGenerations, GrokMediaEndpointVideosEdits, GrokMediaEndpointVideosExtensions:
		meta.ResponseID = extractGrokMediaVideoRequestID(responseBody)
		meta.VideoCount = 1
		meta.VideoResolution = requestInfo.Resolution
		meta.VideoDurationSeconds = requestInfo.DurationSeconds
		// Keep the legacy media-unit counter populated for existing usage displays.
		meta.ImageCount = 1
	}
	return meta
}

func extractGrokMediaVideoRequestID(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	for _, path := range []string{"request_id", "id", "data.request_id", "data.id", "video.request_id", "video.id"} {
		if id := strings.TrimSpace(gjson.GetBytes(body, path).String()); id != "" {
			return id
		}
	}
	return ""
}

// ============================================================================
// OpenAI Sora 兼容响应格式
//
// xAI 原生的视频接口返回的是自己的字段（POST 只有 request_id；GET 状态查询
// 用 status: pending/done/expired/failed，视频地址嵌在 video.url 里，且不回显
// id）。这与 OpenAI 官方 Sora Video API / new-api 等中转程序期望的响应格式不同：
// 后者要求 {id, object, model, status, progress, created_at, ...}，status 取值
// 为 queued/in_progress/completed/failed，且完成后不在状态响应里带 url——而是
// 单独请求 GET /v1/videos/{id}/content 拉取二进制内容。
//
// 下面这组类型和转换函数把 xAI 的原生响应规整成这个标准格式，使 sub2api 的
// Grok 视频接口可以直接被部署为 new-api 等程序里的 "Sora 渠道" 上游。
// ============================================================================

const (
	grokVideoSoraStatusQueued     = "queued"
	grokVideoSoraStatusInProgress = "in_progress"
	grokVideoSoraStatusCompleted  = "completed"
	grokVideoSoraStatusFailed     = "failed"
)

type grokVideoSoraError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

type grokVideoSoraResponse struct {
	ID          string              `json:"id"`
	TaskID      string              `json:"task_id,omitempty"`
	Object      string              `json:"object"`
	Model       string              `json:"model,omitempty"`
	Status      string              `json:"status"`
	Progress    int                 `json:"progress"`
	CreatedAt   int64               `json:"created_at,omitempty"`
	CompletedAt int64               `json:"completed_at,omitempty"`
	Seconds     string              `json:"seconds,omitempty"`
	Size        string              `json:"size,omitempty"`
	// VideoURL 不是 OpenAI Sora 官方字段（官方规范里完成状态不带 URL，需要单独
	// 请求 /content 下载），这里是应中转程序实际使用习惯额外追加的字段，直接
	// 暴露 xAI 返回的视频直链，方便那些不走 /content 端点、而是直接从状态响应
	// 里读 URL 的调用方。
	VideoURL string              `json:"video_url,omitempty"`
	Error    *grokVideoSoraError `json:"error,omitempty"`
}

// grokVideoResolutionToSoraSize 把 Grok 的分辨率档位近似映射成 OpenAI Sora
// 风格的像素尺寸字符串，仅作展示/记录用途（xAI 视频接口本身不接受 size 参数）。
func grokVideoResolutionToSoraSize(resolution string) string {
	switch resolution {
	case VideoBillingResolution1080P:
		return "1920x1080"
	case VideoBillingResolution720P:
		return "1280x720"
	case VideoBillingResolution480P:
		return "854x480"
	default:
		return ""
	}
}

// buildGrokVideoSoraGenerationResponse 把 xAI 提交视频生成后的原生响应
// （仅含 request_id）转换成 OpenAI Sora 标准的 video 对象。xAI 不会在提交
// 响应里回显 model/时长/画质，因此这些字段直接取自客户端的原始请求。
func buildGrokVideoSoraGenerationResponse(requestID, upstreamModel string, requestInfo GrokMediaRequestInfo) []byte {
	resp := grokVideoSoraResponse{
		ID:        requestID,
		TaskID:    requestID,
		Object:    "video",
		Model:     upstreamModel,
		Status:    grokVideoSoraStatusQueued,
		Progress:  0,
		CreatedAt: time.Now().Unix(),
		Size:      grokVideoResolutionToSoraSize(requestInfo.Resolution),
	}
	if requestInfo.DurationSeconds > 0 {
		resp.Seconds = strconv.Itoa(requestInfo.DurationSeconds)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		// 序列化失败时退化为最小可用响应，保证调用方至少能拿到 id 轮询状态。
		out, _ = json.Marshal(grokVideoSoraResponse{ID: requestID, TaskID: requestID, Object: "video", Status: grokVideoSoraStatusQueued})
	}
	return out
}

// mapGrokVideoStatusToSora 把 xAI 原生的 status 取值（pending/done/expired/failed）
// 映射为 OpenAI Sora 标准取值（queued/in_progress/completed/failed）。
func mapGrokVideoStatusToSora(xaiStatus string, progress int) string {
	switch strings.ToLower(strings.TrimSpace(xaiStatus)) {
	case "done", "completed", "success", "succeeded":
		return grokVideoSoraStatusCompleted
	case "expired", "failed", "cancelled", "canceled", "error":
		return grokVideoSoraStatusFailed
	case "queued":
		return grokVideoSoraStatusQueued
	case "pending", "processing", "in_progress", "":
		if progress > 0 {
			return grokVideoSoraStatusInProgress
		}
		return grokVideoSoraStatusQueued
	default:
		// 未知状态时保守地视为仍在处理中，避免把上游一次性的、可轮询恢复的
		// 状态误判为终态失败。
		return grokVideoSoraStatusInProgress
	}
}

// buildGrokVideoSoraStatusResponse 把 xAI GET /v1/videos/{request_id} 的原生
// 响应转换成 OpenAI Sora 标准的 video 对象。xAI 的状态响应里不带 id 字段，
// 用调用方传入的 requestID（也就是客户端轮询时用的那个 id）回填。
func buildGrokVideoSoraStatusResponse(requestID string, xaiBody []byte) []byte {
	xaiStatus := strings.TrimSpace(gjson.GetBytes(xaiBody, "status").String())
	progress := 0
	if p := gjson.GetBytes(xaiBody, "progress"); p.Exists() && p.Type == gjson.Number {
		progress = int(p.Int())
	}
	status := mapGrokVideoStatusToSora(xaiStatus, progress)

	resp := grokVideoSoraResponse{
		ID:       requestID,
		TaskID:   requestID,
		Object:   "video",
		Model:    strings.TrimSpace(gjson.GetBytes(xaiBody, "model").String()),
		Status:   status,
		Progress: progress,
	}

	switch status {
	case grokVideoSoraStatusCompleted:
		if resp.Progress <= 0 {
			resp.Progress = 100
		}
		resp.CompletedAt = time.Now().Unix()
		if duration := gjson.GetBytes(xaiBody, "video.duration"); duration.Exists() {
			if duration.Type == gjson.Number {
				resp.Seconds = strconv.FormatInt(duration.Int(), 10)
			} else {
				resp.Seconds = strings.TrimSpace(duration.String())
			}
		}
		if videoURL := extractGrokMediaVideoDownloadURL(xaiBody); videoURL != "" {
			resp.VideoURL = videoURL
		}
	case grokVideoSoraStatusFailed:
		msg := strings.TrimSpace(gjson.GetBytes(xaiBody, "error.message").String())
		code := strings.TrimSpace(gjson.GetBytes(xaiBody, "error.code").String())
		if msg == "" {
			if strings.EqualFold(xaiStatus, "expired") {
				msg = "video generation request expired"
				if code == "" {
					code = "expired"
				}
			} else {
				msg = "video generation failed"
			}
		}
		resp.Error = &grokVideoSoraError{Message: msg, Code: code}
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return xaiBody
	}
	return out
}

// extractGrokMediaVideoDownloadURL 从 xAI 视频状态响应里取出实际的视频下载地址
// （一个 vidgen.x.ai 上的临时直链），用于实现 GET /v1/videos/{id}/content。
func extractGrokMediaVideoDownloadURL(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	for _, path := range []string{"video.url", "url", "data.video.url", "data.url"} {
		if u := strings.TrimSpace(gjson.GetBytes(body, path).String()); u != "" {
			return u
		}
	}
	return ""
}

// forwardGrokVideoContent 实现 GET /v1/videos/{id}/content：先向 xAI 查询视频
// 生成状态拿到实际的视频直链，再把该直链的二进制内容原样代理/流式返回给
// 调用方（new-api 等中转程序的 Sora 渠道适配器不会读取状态响应里的 url 字段，
// 而是固定请求这个端点来下载视频，因此必须单独实现）。
func (s *OpenAIGatewayService) forwardGrokVideoContent(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	requestID string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		writeGrokMediaErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "request_id is required")
		return nil, fmt.Errorf("grok media video content: request_id is required")
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	statusURL, err := xai.BuildVideoURL(account.GetGrokBaseURL(), requestID)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	statusReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodGet, statusURL, nil)
	if err != nil {
		return nil, err
	}
	statusReq.Header.Set("Authorization", "Bearer "+token)
	statusReq.Header.Set("Accept", "application/json")
	statusReq.Header.Set("User-Agent", "sub2api-grok/1.0")

	statusResp, err := s.httpUpstream.Do(statusReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = statusResp.Body.Close() }()

	if statusResp.StatusCode >= 400 {
		s.updateGrokUsageSnapshot(ctx, account, xai.ParseQuotaHeaders(statusResp.Header, statusResp.StatusCode))
		requestIDHeader := firstNonEmpty(statusResp.Header.Get("x-request-id"), statusResp.Header.Get("xai-request-id"))
		return s.handleGrokMediaErrorResponse(ctx, statusResp, c, account, requestIDHeader, "")
	}
	s.updateGrokUsageSnapshot(ctx, account, xai.ParseQuotaHeaders(statusResp.Header, statusResp.StatusCode))

	statusBody, err := ReadUpstreamResponseBody(statusResp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}

	xaiStatus := strings.TrimSpace(gjson.GetBytes(statusBody, "status").String())
	videoURL := extractGrokMediaVideoDownloadURL(statusBody)
	if !strings.EqualFold(xaiStatus, "done") || videoURL == "" {
		MarkResponseCommitted(c)
		writeGrokMediaErrorResponse(c, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("video is not ready yet (status=%s)", firstNonEmpty(xaiStatus, "unknown")))
		return nil, fmt.Errorf("grok media video content not ready: status=%s", xaiStatus)
	}

	// 临时直链托管在与 api.x.ai 不同的域名（vidgen.x.ai 等），不应把账号的
	// xAI Bearer token 发给第三方主机，这里刻意不带 Authorization 头。
	fileReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodGet, videoURL, nil)
	if err != nil {
		return nil, err
	}
	fileReq.Header.Set("User-Agent", "sub2api-grok/1.0")

	fileResp, err := s.httpUpstream.Do(fileReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = fileResp.Body.Close() }()

	if fileResp.StatusCode >= 400 {
		MarkResponseCommitted(c)
		writeGrokMediaErrorResponse(c, http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("failed to download video content: upstream status %d", fileResp.StatusCode))
		return nil, fmt.Errorf("grok media video content download failed: status %d", fileResp.StatusCode)
	}

	contentType := strings.TrimSpace(fileResp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "video/mp4"
	}
	MarkResponseCommitted(c)
	c.Header("Cache-Control", "private, max-age=3600")
	if cl := strings.TrimSpace(fileResp.Header.Get("Content-Length")); cl != "" {
		c.Header("Content-Length", cl)
	}
	c.Status(http.StatusOK)
	c.Header("Content-Type", contentType)
	if _, err := io.Copy(c.Writer, fileResp.Body); err != nil {
		return nil, err
	}

	return &OpenAIForwardResult{
		RequestID: requestID,
		Duration:  time.Since(startTime),
	}, nil
}

func (s *OpenAIGatewayService) handleGrokMediaErrorResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestIDHeader string,
	requestedModel string,
) (*OpenAIForwardResult, error) {
	body := s.readUpstreamErrorBody(resp)
	// Reconcile readiness before configurable passthrough branches can return;
	// otherwise a Grok 429 can remain schedulable.
	s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body)
	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if upstreamMsg == "" {
		// 结构化字段解析不出内容时（例如上游返回纯文本/HTML 错误页，常见于 415
		// Unsupported Media Type 这类由网关/框架层直接生成的响应），退化为把原始
		// 响应体文本本身当作错误信息返回给客户端，而不是丢弃掉、只给一个无意义
		// 的 "xAI upstream returned status NNN"。
		if raw := strings.TrimSpace(string(body)); raw != "" {
			upstreamMsg = sanitizeUpstreamErrorMessage(truncateString(raw, 500))
		}
	}
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("xAI upstream returned status %d", resp.StatusCode)
	}

	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)

	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		account.Platform,
		resp.StatusCode,
		body,
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	); matched {
		MarkResponseCommitted(c)
		writeGrokMediaErrorResponse(c, status, errType, errMsg)
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}

	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  requestIDHeader,
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		writeGrokMediaErrorResponse(c, http.StatusInternalServerError, "upstream_error", "Upstream gateway error")
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	kind := "http_error"
	if s.shouldFailoverUpstreamError(resp.StatusCode) {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  requestIDHeader,
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if kind == "failover" {
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           body,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	MarkResponseCommitted(c)
	writeGrokMediaErrorResponse(c, resp.StatusCode, grokMediaErrorType(resp.StatusCode), upstreamMsg)
	return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
}

func grokMediaErrorType(statusCode int) string {
	switch statusCode {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "upstream_error"
	}
}

func writeGrokMediaErrorResponse(c *gin.Context, statusCode int, errType, message string) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    strings.TrimSpace(errType),
			"message": strings.TrimSpace(message),
		},
	})
}

func writeGrokMediaResponse(c *gin.Context, resp *http.Response, body []byte, filter *responseheaders.CompiledHeaderFilter) {
	if c == nil || resp == nil {
		return
	}
	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, filter)
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(resp.StatusCode, contentType, body)
}
