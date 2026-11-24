package service

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"runtime"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	xdraw "golang.org/x/image/draw"
)

// codex 直调通道只接受画幅比例，实际像素由上游固定（约 1.6K）。网关按请求声明的
// 档位与比例在结果返回前重新采样，使 2K / 4K 档能拿到对应像素。
const (
	codexImageLongEdge2K = 2048
	codexImageLongEdge4K = 3840

	// codexImageDefaultLongEdge 是只声明比例、未声明档位时的输出长边（与网关默认 2K 计费一致）。
	codexImageDefaultLongEdge = codexImageLongEdge2K

	// codexImageResizeJPEGQuality 控制非透明结果的编码质量。4K PNG 单张约 13 MB，
	// 超过多数客户端的响应体上限，默认转 JPEG；透明背景仍保留 PNG。
	codexImageResizeJPEGQuality = 94

	// codexImageResizeMaxConcurrency 是重采样的并发上限。4 核实测（GOMAXPROCS=4）：
	// 4K 16:9 在 conc=4 达 8.61 img/s、1:1 达 6.05 img/s，conc=6 两者都回落，
	// 说明 4 核的拐点就是 4。上限留 6 供更多核机器使用，但不再往上加。
	codexImageResizeMaxConcurrency = 6

	// codexImageResizeTolerance 是判定「已经是目标尺寸」的容差像素。
	codexImageResizeTolerance = 2
)

// codexImageResizeWorkerLimit 按可用 CPU 决定重采样并发，并受上限约束：
// 容器里 GOMAXPROCS 会跟随 cgroup 配额，比 NumCPU 更贴近真实算力。
func codexImageResizeWorkerLimit() int {
	limit := runtime.GOMAXPROCS(0)
	if limit > codexImageResizeMaxConcurrency {
		limit = codexImageResizeMaxConcurrency
	}
	if limit < 2 {
		limit = 2
	}
	return limit
}

var codexImageResizeSem = make(chan struct{}, codexImageResizeWorkerLimit())

// codexImageResizeTarget 描述请求期望的输出规格。
// width/height 非零时表示客户端直接给了像素；其余情况按 longEdge + 比例推导。
type codexImageResizeTarget struct {
	longEdge int
	aspectW  int
	aspectH  int
	width    int
	height   int
}

// resolveCodexImageResizeTarget 解析 size 参数，支持四种写法：
//
//	4k              档位，画幅跟随上游
//	4k:16:9         档位 + 比例
//	16:9            纯比例，按默认 2K 处理
//	3840x2160       像素，直接作为目标尺寸
//
// 1K 档与 auto 返回 false：1K 不做重采样，维持上游原始输出。
func resolveCodexImageResizeTarget(size string) (codexImageResizeTarget, bool) {
	raw := strings.ToLower(strings.TrimSpace(size))
	if raw == "" || raw == "auto" {
		return codexImageResizeTarget{}, false
	}
	if width, height, ok := parseImageBillingDimensions(raw); ok {
		if tier, classified := ClassifyImageBillingTier(raw); classified && tier == ImageBillingSize1K {
			return codexImageResizeTarget{}, false
		}
		return codexImageResizeTarget{
			width:    width &^ 1,
			height:   height &^ 1,
			longEdge: max(width, height),
		}, true
	}

	tier := ""
	rest := raw
	for _, candidate := range []string{"1k", "2k", "4k"} {
		if strings.HasPrefix(raw, candidate) {
			tier = strings.ToUpper(candidate)
			rest = strings.TrimPrefix(raw, candidate)
			rest = strings.Trim(strings.TrimSpace(rest), ":")
			break
		}
	}
	if tier == ImageBillingSize1K {
		return codexImageResizeTarget{}, false
	}
	if tier == "" {
		aspectW, aspectH, ok := parseImageAspectRatio(raw)
		if !ok {
			return codexImageResizeTarget{}, false
		}
		return codexImageResizeTarget{
			longEdge: codexImageDefaultLongEdge,
			aspectW:  aspectW,
			aspectH:  aspectH,
		}, true
	}
	longEdge := codexImageLongEdgeForTier(tier)
	if longEdge == 0 {
		return codexImageResizeTarget{}, false
	}
	if rest == "" || rest == "auto" {
		return codexImageResizeTarget{longEdge: longEdge}, true
	}
	aspectW, aspectH, ok := parseImageAspectRatio(rest)
	if !ok {
		return codexImageResizeTarget{}, false
	}
	return codexImageResizeTarget{longEdge: longEdge, aspectW: aspectW, aspectH: aspectH}, true
}

func codexImageLongEdgeForTier(tier string) int {
	switch tier {
	case ImageBillingSize4K:
		return codexImageLongEdge4K
	case ImageBillingSize2K:
		return codexImageLongEdge2K
	default:
		return 0
	}
}

// parseImageAspectRatio 解析 "16:9" / "1:1" 这类比例，两边都必须是正整数。
func parseImageAspectRatio(value string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	width, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || width <= 0 {
		return 0, 0, false
	}
	height, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// pixelSize 按目标规格算出最终输出尺寸；未声明比例时沿用源图比例。
func (t codexImageResizeTarget) pixelSize(sourceW, sourceH int) (int, int) {
	if t.width > 0 && t.height > 0 {
		return t.width, t.height
	}
	aspectW, aspectH := t.aspectW, t.aspectH
	if aspectW <= 0 || aspectH <= 0 {
		aspectW, aspectH = sourceW, sourceH
	}
	if aspectW <= 0 || aspectH <= 0 || t.longEdge <= 0 {
		return 0, 0
	}
	if aspectW >= aspectH {
		height := int(math.Round(float64(t.longEdge) * float64(aspectH) / float64(aspectW)))
		return t.longEdge &^ 1, max(2, height&^1)
	}
	width := int(math.Round(float64(t.longEdge) * float64(aspectW) / float64(aspectH)))
	return max(2, width&^1), t.longEdge &^ 1
}

// upstreamCodexImageSize 把请求里的 size 归一成上游能理解的写法。
// codex 通道只认比例或像素，带档位前缀的写法会让上游退回默认画幅。
func upstreamCodexImageSize(size string) (string, bool) {
	target, ok := resolveCodexImageResizeTarget(size)
	if !ok {
		return "", false
	}
	if target.aspectW > 0 && target.aspectH > 0 {
		return fmt.Sprintf("%d:%d", target.aspectW, target.aspectH), true
	}
	if target.width > 0 && target.height > 0 {
		return fmt.Sprintf("%dx%d", target.width, target.height), true
	}
	// 只有档位、没有画幅要求：交给上游按提示词决定。
	return "", false
}

// normalizeCodexImageQuality 把客户端传入的 quality 归一成上游认得的四档。
// 未知取值返回 false，由调用方丢弃，避免把非法值透传给上游触发 400。
// standard / hd 是 DALL·E 3 时代的写法，一并兼容。
func normalizeCodexImageQuality(quality string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "":
		return "", false
	case "low", "standard":
		return "low", true
	case "medium":
		return "medium", true
	case "high", "hd":
		return "high", true
	case "max":
		return "max", true
	default:
		return "", false
	}
}

// codexImageEncodeOptions 是重采样后的编码参数。质量档同时决定 JPEG 质量与锐化强度：
// low 追求体积与速度，max 追求纹理，不影响上游出图分辨率。
type codexImageEncodeOptions struct {
	keepPNG     bool
	jpegQuality int
	sharpness   float32
}

func codexImageEncodeOptionsFor(parsed *OpenAIImagesRequest) codexImageEncodeOptions {
	opts := codexImageEncodeOptions{
		jpegQuality: codexImageResizeJPEGQuality,
		sharpness:   0.55,
	}
	if parsed == nil {
		return opts
	}
	opts.keepPNG = strings.EqualFold(strings.TrimSpace(parsed.Background), "transparent")
	switch quality, _ := normalizeCodexImageQuality(parsed.Quality); quality {
	case "low":
		opts.jpegQuality = 80
		opts.sharpness = 0.35
	case "medium":
		opts.jpegQuality = 88
		opts.sharpness = 0.45
	case "max":
		opts.jpegQuality = 97
		opts.sharpness = 0.65
	}
	return opts
}

// resizeCodexImagesBody 就地把响应体里每张 b64_json 重采样到请求档位，并同步改写尺寸字段。
func resizeCodexImagesBody(body []byte, parsed *OpenAIImagesRequest) ([]byte, bool) {
	if parsed == nil {
		return body, false
	}
	target, ok := resolveCodexImageResizeTarget(parsed.Size)
	if !ok {
		return body, false
	}
	items := gjson.GetBytes(body, "data")
	if !items.IsArray() {
		return body, false
	}
	opts := codexImageEncodeOptionsFor(parsed)
	changed := false
	for i, item := range items.Array() {
		encoded := strings.TrimSpace(item.Get("b64_json").String())
		if encoded == "" {
			continue
		}
		result, err := resizeCodexImageB64(encoded, target, opts)
		if err != nil || !result.changed {
			continue
		}
		size := fmt.Sprintf("%dx%d", result.width, result.height)
		body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.b64_json", i), result.encoded)
		body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.size", i), size)
		body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.output_format", i), result.format)
		if i == 0 {
			body, _ = sjson.SetBytes(body, "size", size)
			body, _ = sjson.SetBytes(body, "output_format", result.format)
		}
		changed = true
	}
	return body, changed
}

// resizeCodexImagesEvent 处理 SSE 单条事件里的图片。事件可能直接带 b64_json，
// 也可能沿用 data 数组结构，两种都覆盖。
func resizeCodexImagesEvent(data []byte, parsed *OpenAIImagesRequest) ([]byte, bool) {
	if parsed == nil {
		return data, false
	}
	if gjson.GetBytes(data, "data").IsArray() {
		return resizeCodexImagesBody(data, parsed)
	}
	target, ok := resolveCodexImageResizeTarget(parsed.Size)
	if !ok {
		return data, false
	}
	encoded := strings.TrimSpace(gjson.GetBytes(data, "b64_json").String())
	if encoded == "" {
		return data, false
	}
	result, err := resizeCodexImageB64(encoded, target, codexImageEncodeOptionsFor(parsed))
	if err != nil || !result.changed {
		return data, false
	}
	size := fmt.Sprintf("%dx%d", result.width, result.height)
	data, _ = sjson.SetBytes(data, "b64_json", result.encoded)
	data, _ = sjson.SetBytes(data, "size", size)
	data, _ = sjson.SetBytes(data, "output_format", result.format)
	return data, true
}

type codexImageResizeResult struct {
	encoded string
	format  string
	width   int
	height  int
	changed bool
}

// resizeCodexImageB64 把单张图片重采样到目标尺寸。比例不一致时按 cover 策略取中心最大区域，
// 保证输出比例严格等于请求比例，不做留边。
func resizeCodexImageB64(encoded string, target codexImageResizeTarget, opts codexImageEncodeOptions) (codexImageResizeResult, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return codexImageResizeResult{}, fmt.Errorf("decode b64_json: %w", err)
	}
	src, srcFormat, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return codexImageResizeResult{}, fmt.Errorf("decode image: %w", err)
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return codexImageResizeResult{}, fmt.Errorf("invalid image bounds %dx%d", width, height)
	}
	targetW, targetH := target.pixelSize(width, height)
	if targetW <= 0 || targetH <= 0 {
		return codexImageResizeResult{}, nil
	}
	if absInt(width-targetW) <= codexImageResizeTolerance && absInt(height-targetH) <= codexImageResizeTolerance {
		return codexImageResizeResult{
			encoded: encoded,
			format:  srcFormat,
			width:   width,
			height:  height,
		}, nil
	}

	codexImageResizeSem <- struct{}{}
	defer func() { <-codexImageResizeSem }()

	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, coverSourceRect(bounds, targetW, targetH), xdraw.Src, nil)
	// 只在放大时锐化；缩小时锐化会放大锯齿。
	sharpness := opts.sharpness
	if targetW <= width && targetH <= height {
		sharpness = 0
	}
	if sharpness > 0 {
		sharpenRGBA(dst, sharpness)
	}

	keepPNG := opts.keepPNG || srcFormat == "png" && hasTransparentPixel(src)
	var buf bytes.Buffer
	format := "jpeg"
	if keepPNG {
		format = "png"
		if err := png.Encode(&buf, dst); err != nil {
			return codexImageResizeResult{}, fmt.Errorf("encode png: %w", err)
		}
	} else if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: opts.jpegQuality}); err != nil {
		return codexImageResizeResult{}, fmt.Errorf("encode jpeg: %w", err)
	}
	return codexImageResizeResult{
		encoded: base64.StdEncoding.EncodeToString(buf.Bytes()),
		format:  format,
		width:   targetW,
		height:  targetH,
		changed: true,
	}, nil
}

// coverSourceRect 返回源图中与目标比例一致的中心最大区域（cover 裁切）。
// 比例差异小于 0.5% 时直接用整图，避免为 1 px 误差裁掉边缘。
func coverSourceRect(bounds image.Rectangle, targetW, targetH int) image.Rectangle {
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 || targetW <= 0 || targetH <= 0 {
		return bounds
	}
	targetRatio := float64(targetW) / float64(targetH)
	currentRatio := float64(width) / float64(height)
	if math.Abs(currentRatio-targetRatio)/targetRatio < 0.005 {
		return bounds
	}
	if currentRatio > targetRatio {
		croppedWidth := int(math.Round(float64(height) * targetRatio))
		if croppedWidth < 1 {
			croppedWidth = 1
		}
		x0 := bounds.Min.X + (width-croppedWidth)/2
		return image.Rect(x0, bounds.Min.Y, x0+croppedWidth, bounds.Max.Y)
	}
	croppedHeight := int(math.Round(float64(width) / targetRatio))
	if croppedHeight < 1 {
		croppedHeight = 1
	}
	y0 := bounds.Min.Y + (height-croppedHeight)/2
	return image.Rect(bounds.Min.X, y0, bounds.Max.X, y0+croppedHeight)
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// hasTransparentPixel 抽样检查图片是否存在非完全不透明像素。整图逐像素扫描 4K 数据
// 代价偏高，这里按稀疏网格采样：插画类透明背景只要边缘有透明就会被采样命中。
func hasTransparentPixel(img image.Image) bool {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return false
	}
	stepX := max(1, width/64)
	stepY := max(1, height/64)
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			_, _, _, alpha := img.At(x, y).RGBA()
			if alpha < 0xffff {
				return true
			}
		}
	}
	return false
}

// sharpenRGBA 做一次轻量反锐化：按五邻域拉普拉斯提升边缘对比，
// 抵消 CatmullRom 放大带来的软化，避免出现明显的插值糊感。
func sharpenRGBA(img *image.RGBA, amount float32) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < 3 || height < 3 {
		return
	}
	src := make([]uint8, len(img.Pix))
	copy(src, img.Pix)
	stride := img.Stride
	for y := 1; y < height-1; y++ {
		row := y * stride
		for x := 1; x < width-1; x++ {
			i := row + x*4
			for c := 0; c < 3; c++ {
				center := float32(src[i+c])
				neighbors := float32(src[i-4+c]) + float32(src[i+4+c]) +
					float32(src[i-stride+c]) + float32(src[i+stride+c])
				value := center + amount*(4*center-neighbors)
				if value < 0 {
					value = 0
				} else if value > 255 {
					value = 255
				}
				img.Pix[i+c] = uint8(value)
			}
		}
	}
}
