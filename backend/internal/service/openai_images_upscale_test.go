package service

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"runtime"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func encodeTestPNG(t *testing.T, width, height int, transparent bool) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			alpha := uint8(255)
			if transparent && x < 8 {
				alpha = 0
			}
			img.SetRGBA(x, y, color.RGBA{uint8(x % 255), uint8(y % 255), 96, alpha})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestClassifyImageBillingTierSupportsTierPrefix(t *testing.T) {
	cases := map[string]string{
		"4k:16:9": ImageBillingSize4K,
		"4K:1:1":  ImageBillingSize4K,
		"2k:4:3":  ImageBillingSize2K,
		"1k:9:16": ImageBillingSize1K,
		"4k":      ImageBillingSize4K,
		"2k":      ImageBillingSize2K,
	}
	for size, want := range cases {
		got, ok := ClassifyImageBillingTier(size)
		if !ok || got != want {
			t.Errorf("ClassifyImageBillingTier(%q) = %q,%v, want %q", size, got, ok, want)
		}
	}
}

func TestResolveCodexImageResizeTarget(t *testing.T) {
	cases := []struct {
		size     string
		ok       bool
		longEdge int
		aspectW  int
		aspectH  int
		width    int
		height   int
	}{
		{size: "4k", ok: true, longEdge: codexImageLongEdge4K},
		{size: "4k:16:9", ok: true, longEdge: codexImageLongEdge4K, aspectW: 16, aspectH: 9},
		{size: "4k:1:1", ok: true, longEdge: codexImageLongEdge4K, aspectW: 1, aspectH: 1},
		{size: "2k:4:3", ok: true, longEdge: codexImageLongEdge2K, aspectW: 4, aspectH: 3},
		{size: "16:9", ok: true, longEdge: codexImageDefaultLongEdge, aspectW: 16, aspectH: 9},
		{size: "9:16", ok: true, longEdge: codexImageDefaultLongEdge, aspectW: 9, aspectH: 16},
		{size: "3840x2160", ok: true, width: 3840, height: 2160, longEdge: 3840},
		{size: "2049x2049", ok: true, width: 2048, height: 2048, longEdge: 2049},
		{size: "1k", ok: true, longEdge: codexImageLongEdge1K},
		{size: "1k:1:1", ok: true, longEdge: codexImageLongEdge1K, aspectW: 1, aspectH: 1},
		{size: "1024x1024", ok: true, width: 1024, height: 1024, longEdge: 1024},
		{size: "auto", ok: false},
		{size: "", ok: false},
		{size: "garbage", ok: false},
	}
	for _, tc := range cases {
		got, ok := resolveCodexImageResizeTarget(tc.size)
		if ok != tc.ok {
			t.Errorf("resolve(%q) ok = %v, want %v", tc.size, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.longEdge != tc.longEdge || got.aspectW != tc.aspectW ||
			got.aspectH != tc.aspectH || got.width != tc.width || got.height != tc.height {
			t.Errorf("resolve(%q) = %+v, want longEdge=%d aspect=%d:%d px=%dx%d",
				tc.size, got, tc.longEdge, tc.aspectW, tc.aspectH, tc.width, tc.height)
		}
	}
}

func TestCodexImageResizeTargetPixelSize(t *testing.T) {
	cases := []struct {
		target   codexImageResizeTarget
		srcW     int
		srcH     int
		wantW    int
		wantH    int
		nameHint string
	}{
		{codexImageResizeTarget{longEdge: 3840, aspectW: 1, aspectH: 1}, 1672, 941, 3840, 3840, "4k 1:1"},
		{codexImageResizeTarget{longEdge: 3840, aspectW: 4, aspectH: 3}, 1672, 941, 3840, 2880, "4k 4:3"},
		{codexImageResizeTarget{longEdge: 3840, aspectW: 16, aspectH: 9}, 1672, 941, 3840, 2160, "4k 16:9"},
		{codexImageResizeTarget{longEdge: 3840, aspectW: 9, aspectH: 16}, 1672, 941, 2160, 3840, "4k 9:16"},
		{codexImageResizeTarget{longEdge: 3840, aspectW: 3, aspectH: 4}, 1672, 941, 2880, 3840, "4k 3:4"},
		{codexImageResizeTarget{longEdge: 2048, aspectW: 16, aspectH: 9}, 1672, 941, 2048, 1152, "2k 16:9"},
		{codexImageResizeTarget{longEdge: 2048, aspectW: 1, aspectH: 1}, 1672, 941, 2048, 2048, "2k 1:1"},
		{codexImageResizeTarget{longEdge: 1024, aspectW: 16, aspectH: 9}, 1672, 941, 1024, 576, "1k 16:9"},
		{codexImageResizeTarget{longEdge: 1024, aspectW: 1, aspectH: 1}, 1672, 941, 1024, 1024, "1k 1:1"},
		{codexImageResizeTarget{longEdge: 1024}, 1672, 941, 1024, 576, "1k 跟随源比例"},
		{codexImageResizeTarget{longEdge: 3840}, 1672, 941, 3840, 2160, "4k 跟随源比例"},
		{codexImageResizeTarget{width: 4096, height: 2304}, 800, 600, 4096, 2304, "显式像素"},
	}
	for _, tc := range cases {
		gotW, gotH := tc.target.pixelSize(tc.srcW, tc.srcH)
		if gotW != tc.wantW || gotH != tc.wantH {
			t.Errorf("%s: pixelSize = %dx%d, want %dx%d", tc.nameHint, gotW, gotH, tc.wantW, tc.wantH)
		}
	}
}

func TestUpstreamCodexImageSize(t *testing.T) {
	cases := []struct {
		size string
		want string
		ok   bool
	}{
		{"4k:16:9", "16:9", true},
		{"4k:1:1", "1:1", true},
		{"2k:9:16", "9:16", true},
		{"16:9", "16:9", true},
		{"3840x2160", "3840x2160", true},
		{"4k", "", false},
		{"1k", "", false},
		{"1024x1024", "1024x1024", true},
		{"1k:16:9", "16:9", true},
		{"auto", "", false},
	}
	for _, tc := range cases {
		got, ok := upstreamCodexImageSize(tc.size)
		if got != tc.want || ok != tc.ok {
			t.Errorf("upstreamCodexImageSize(%q) = %q,%v, want %q,%v", tc.size, got, ok, tc.want, tc.ok)
		}
	}
}

func TestResizeCodexImageB64To1K(t *testing.T) {
	encoded := encodeTestPNG(t, 1672, 941, false)
	target, ok := resolveCodexImageResizeTarget("1k:16:9")
	if !ok {
		t.Fatal("1k:16:9 should be resized")
	}
	result, err := resizeCodexImageB64(encoded, target, testEncodeOptions(false))
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if !result.changed {
		t.Fatal("expected image to be downscaled")
	}
	if result.width != 1024 || result.height != 576 {
		t.Fatalf("got %dx%d, want 1024x576", result.width, result.height)
	}
	// 输出尺寸必须真的落在 1K 计费档，否则价格与规格对不上。
	if tier, ok := ClassifyImageBillingTier(fmt.Sprintf("%dx%d", result.width, result.height)); !ok || tier != ImageBillingSize1K {
		t.Fatalf("output %dx%d classified as %q,%v, want 1K", result.width, result.height, tier, ok)
	}
}

func TestResizeCodexImagesBodyTo1K(t *testing.T) {
	encoded := encodeTestPNG(t, 1254, 1254, false)
	body := []byte(`{"data":[{"b64_json":"` + encoded + `"}]}`)
	updated, changed := resizeCodexImagesBody(body, &OpenAIImagesRequest{Size: "1k:1:1"})
	if !changed {
		t.Fatal("expected 1k body to be rewritten")
	}
	if got := gjson.GetBytes(updated, "data.0.size").String(); got != "1024x1024" {
		t.Fatalf("data.0.size = %q, want 1024x1024", got)
	}
}

func TestResizeCodexImageB64To2K(t *testing.T) {
	encoded := encodeTestPNG(t, 800, 450, false)
	target, _ := resolveCodexImageResizeTarget("2k:16:9")
	result, err := resizeCodexImageB64(encoded, target, testEncodeOptions(false))
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if !result.changed {
		t.Fatal("expected image to be resized")
	}
	if result.width != 2048 || result.height != 1152 {
		t.Fatalf("got %dx%d, want 2048x1152", result.width, result.height)
	}
	if result.format != "jpeg" {
		t.Fatalf("got format %q, want jpeg", result.format)
	}
}

func TestResizeCodexImageB64To4KSquare(t *testing.T) {
	encoded := encodeTestPNG(t, 900, 600, false)
	target, _ := resolveCodexImageResizeTarget("4k:1:1")
	result, err := resizeCodexImageB64(encoded, target, testEncodeOptions(false))
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if result.width != 3840 || result.height != 3840 {
		t.Fatalf("got %dx%d, want 3840x3840", result.width, result.height)
	}
	decoded, err := decodeBase64Image(result.encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := decoded.Bounds(); got.Dx() != 3840 || got.Dy() != 3840 {
		t.Fatalf("decoded bounds %v, want 3840x3840", got)
	}
}

func TestResizeCodexImageB64Portrait(t *testing.T) {
	encoded := encodeTestPNG(t, 1200, 800, false)
	target, _ := resolveCodexImageResizeTarget("4k:9:16")
	result, err := resizeCodexImageB64(encoded, target, testEncodeOptions(false))
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if result.width != 2160 || result.height != 3840 {
		t.Fatalf("got %dx%d, want 2160x3840", result.width, result.height)
	}
}

func TestResizeCodexImageB64TransparentKeepsPNG(t *testing.T) {
	encoded := encodeTestPNG(t, 640, 640, true)
	target, _ := resolveCodexImageResizeTarget("4k:1:1")
	result, err := resizeCodexImageB64(encoded, target, testEncodeOptions(true))
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if result.format != "png" {
		t.Fatalf("got format %q, want png", result.format)
	}
	if result.width != 3840 || result.height != 3840 {
		t.Fatalf("got %dx%d, want 3840x3840", result.width, result.height)
	}
}

func TestResizeCodexImageB64SkipsWhenSizeMatches(t *testing.T) {
	encoded := encodeTestPNG(t, 2048, 1152, false)
	target, _ := resolveCodexImageResizeTarget("2k:16:9")
	result, err := resizeCodexImageB64(encoded, target, testEncodeOptions(false))
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if result.changed {
		t.Fatal("expected image at target size to be left untouched")
	}
	if result.encoded != encoded {
		t.Fatal("expected original payload to be preserved")
	}
}

func TestResizeCodexImagesBody(t *testing.T) {
	encoded := encodeTestPNG(t, 800, 600, false)
	body := []byte(`{"created":1,"data":[{"b64_json":"` + encoded + `","size":"800x600"}]}`)
	parsed := &OpenAIImagesRequest{Size: "4k:4:3"}

	updated, changed := resizeCodexImagesBody(body, parsed)
	if !changed {
		t.Fatal("expected body to be rewritten")
	}
	if got := gjson.GetBytes(updated, "data.0.size").String(); got != "3840x2880" {
		t.Fatalf("data.0.size = %q, want 3840x2880", got)
	}
	if got := gjson.GetBytes(updated, "size").String(); got != "3840x2880" {
		t.Fatalf("size = %q, want 3840x2880", got)
	}
	if got := gjson.GetBytes(updated, "data.0.output_format").String(); got != "jpeg" {
		t.Fatalf("data.0.output_format = %q, want jpeg", got)
	}
	if gjson.GetBytes(updated, "data.0.b64_json").String() == encoded {
		t.Fatal("expected b64_json to be replaced")
	}
	if gjson.GetBytes(updated, "created").Int() != 1 {
		t.Fatal("unrelated field was lost")
	}
}

func TestResizeCodexImagesBodySkipsUntouchedRequests(t *testing.T) {
	encoded := encodeTestPNG(t, 320, 240, false)
	body := []byte(`{"data":[{"b64_json":"` + encoded + `"}]}`)
	for _, size := range []string{"auto", ""} {
		updated, changed := resizeCodexImagesBody(body, &OpenAIImagesRequest{Size: size})
		if changed {
			t.Errorf("size %q: expected no rewrite", size)
		}
		if !bytes.Equal(updated, body) {
			t.Errorf("size %q: expected body unchanged", size)
		}
	}
	if _, changed := resizeCodexImagesBody(body, nil); changed {
		t.Fatal("expected nil request to be untouched")
	}
}

func TestResizeCodexImagesEvent(t *testing.T) {
	encoded := encodeTestPNG(t, 512, 512, false)
	event := []byte(`{"type":"image_generation.completed","b64_json":"` + encoded + `"}`)
	updated, changed := resizeCodexImagesEvent(event, &OpenAIImagesRequest{Size: "4k"})
	if !changed {
		t.Fatal("expected event to be rewritten")
	}
	if got := gjson.GetBytes(updated, "size").String(); got != "3840x3840" {
		t.Fatalf("size = %q, want 3840x3840", got)
	}
	if got := gjson.GetBytes(updated, "type").String(); got != "image_generation.completed" {
		t.Fatalf("type changed to %q", got)
	}
}

func TestResizeCodexImagesBodyViaSJSON(t *testing.T) {
	encoded := encodeTestPNG(t, 256, 256, false)
	body, _ := sjson.SetBytes([]byte(`{"created":1}`), "data", []map[string]any{{"b64_json": encoded}})
	updated, changed := resizeCodexImagesBody(body, &OpenAIImagesRequest{Size: "2k:1:1"})
	if !changed {
		t.Fatal("expected body to be rewritten")
	}
	if !gjson.ValidBytes(updated) {
		t.Fatal("rewritten body is not valid JSON")
	}
	if gjson.GetBytes(updated, "created").Int() != 1 {
		t.Fatal("unrelated field was lost")
	}
	if got := gjson.GetBytes(updated, "size").String(); got != "2048x2048" {
		t.Fatalf("size = %q, want 2048x2048", got)
	}
}

func TestCoverSourceRect(t *testing.T) {
	bounds := image.Rect(0, 0, 900, 600)
	// 3:2 源要 1:1 → 取中心 600x600。
	if got := coverSourceRect(bounds, 3840, 3840); got != image.Rect(150, 0, 750, 600) {
		t.Fatalf("coverSourceRect 1:1 = %v, want (150,0)-(750,600)", got)
	}
	// 3:2 源要 16:9 → 高度裁到 506。
	if got := coverSourceRect(bounds, 3840, 2160); got.Dx() != 900 || got.Dy() != 506 {
		t.Fatalf("coverSourceRect 16:9 = %v, want 900x506", got)
	}
	// 比例几乎一致时直接用整图。
	if got := coverSourceRect(image.Rect(0, 0, 1672, 941), 3840, 2160); got != image.Rect(0, 0, 1672, 941) {
		t.Fatalf("coverSourceRect near-match = %v, want full bounds", got)
	}
}

func testEncodeOptions(keepPNG bool) codexImageEncodeOptions {
	return codexImageEncodeOptions{keepPNG: keepPNG, jpegQuality: codexImageResizeJPEGQuality, sharpness: 0.55}
}

func decodeBase64Image(encoded string) (image.Image, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	return src, err
}

func TestNormalizeCodexImageQuality(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"low", "low", true},
		{"LOW", "low", true},
		{"standard", "low", true},
		{"medium", "medium", true},
		{"high", "high", true},
		{"hd", "high", true},
		{"max", "max", true},
		{" Max ", "max", true},
		{"", "", false},
		{"ultra", "", false},
		{"3", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeCodexImageQuality(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("normalizeCodexImageQuality(%q) = %q,%v, want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCodexImageEncodeOptionsForQuality(t *testing.T) {
	cases := map[string]int{
		"low":    80,
		"medium": 88,
		"high":   codexImageResizeJPEGQuality,
		"max":    97,
		"":       codexImageResizeJPEGQuality,
		"bogus":  codexImageResizeJPEGQuality,
	}
	for quality, want := range cases {
		opts := codexImageEncodeOptionsFor(&OpenAIImagesRequest{Quality: quality})
		if opts.jpegQuality != want {
			t.Errorf("quality %q: jpegQuality = %d, want %d", quality, opts.jpegQuality, want)
		}
	}
	if opts := codexImageEncodeOptionsFor(&OpenAIImagesRequest{Quality: "max"}); opts.sharpness <= 0.55 {
		t.Errorf("max sharpness = %v, want > 0.55", opts.sharpness)
	}
	if opts := codexImageEncodeOptionsFor(&OpenAIImagesRequest{Quality: "low"}); opts.sharpness >= 0.55 {
		t.Errorf("low sharpness = %v, want < 0.55", opts.sharpness)
	}
	if opts := codexImageEncodeOptionsFor(&OpenAIImagesRequest{Background: "transparent"}); !opts.keepPNG {
		t.Error("transparent background should keep png")
	}
}

func TestResizeQualityChangesPayloadSize(t *testing.T) {
	encoded := encodeTestPNG(t, 900, 600, false)
	target, _ := resolveCodexImageResizeTarget("4k:16:9")
	low, err := resizeCodexImageB64(encoded, target, codexImageEncodeOptionsFor(&OpenAIImagesRequest{Quality: "low"}))
	if err != nil {
		t.Fatalf("low: %v", err)
	}
	max, err := resizeCodexImageB64(encoded, target, codexImageEncodeOptionsFor(&OpenAIImagesRequest{Quality: "max"}))
	if err != nil {
		t.Fatalf("max: %v", err)
	}
	if len(low.encoded) >= len(max.encoded) {
		t.Fatalf("low payload %d bytes should be smaller than max %d bytes", len(low.encoded), len(max.encoded))
	}
}

func TestCodexImageResizeWorkerLimit(t *testing.T) {
	previous := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	cases := map[int]int{
		1:  2, // 单核也保留 2 个槽位，避免串行化把小请求拖长
		2:  2,
		4:  4, // 目标服务器：4 核给 4，实测拐点
		8:  6, // 超过上限按 6 封顶
		16: 6,
	}
	for procs, want := range cases {
		runtime.GOMAXPROCS(procs)
		if got := codexImageResizeWorkerLimit(); got != want {
			t.Errorf("GOMAXPROCS=%d: workerLimit = %d, want %d", procs, got, want)
		}
	}
}
