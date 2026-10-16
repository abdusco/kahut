package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func fakeOpenRouter(t *testing.T, status int, body string) (*openRouter, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(r.Context())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &openRouter{key: "k", model: "m/img", base: srv.URL, client: srv.Client()}, &got
}

func imageReply(b64 string) string {
	b, _ := json.Marshal(map[string]any{"data": []any{map[string]string{"b64_json": b64}}})
	return string(b)
}

func TestOpenRouterGenerate(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("pixels"))
	o, req := fakeOpenRouter(t, 200, imageReply(png))
	data, err := o.Generate(context.Background(), "a cat")
	if err != nil || string(data) != "pixels" {
		t.Fatalf("got %q, %v", data, err)
	}
	if req.Header.Get("Authorization") != "Bearer k" || req.URL.Path != "/images" {
		t.Fatalf("bad request: %s %v", req.URL.Path, req.Header)
	}
}

func noisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	noise := rand.New(rand.NewPCG(1, 2))
	for y := range h {
		for x := range w {
			src.Set(x, y, color.NRGBA{R: uint8(noise.IntN(256)), G: uint8(noise.IntN(256)), B: uint8(noise.IntN(256)), A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, src); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestNormalizeImageResizes(t *testing.T) {
	dims := func(data []byte) (string, int, int) {
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		return format, cfg.Width, cfg.Height
	}
	for _, c := range []struct{ w, h, wantW, wantH int }{
		{1024, 1024, 600, 600}, // square
		{1888, 1216, 800, 515}, // landscape, limited by width
		{832, 1280, 520, 800},  // portrait, 600x800 box limited by height
		{1344, 768, 800, 457},  // 16:9
		{400, 300, 400, 300},   // already small: PNG still becomes JPEG, same size
	} {
		format, w, h := dims(normalizeImage(noisePNG(t, c.w, c.h)))
		if format != "jpeg" || abs(w-c.wantW) > 1 || abs(h-c.wantH) > 1 {
			t.Errorf("%dx%d -> %s %dx%d, want jpeg %dx%d", c.w, c.h, format, w, h, c.wantW, c.wantH)
		}
		if w*h >= 1_000_000 {
			t.Errorf("%dx%d -> %d pixels, want under 1 MP", c.w, c.h, w*h)
		}
	}
	// A JPEG that already fits is returned byte for byte.
	small := normalizeImage(noisePNG(t, 300, 200))
	if got := normalizeImage(small); !bytes.Equal(got, small) {
		t.Error("fitting JPEG was re-encoded")
	}
	// A large JPEG is shrunk.
	var big bytes.Buffer
	_ = jpeg.Encode(&big, image.NewRGBA(image.Rect(0, 0, 1600, 1200)), nil)
	if _, w, h := dims(normalizeImage(big.Bytes())); w != 800 || h != 600 {
		t.Errorf("JPEG shrunk to %dx%d", w, h)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestPNGToJPEG(t *testing.T) {
	// Noisy, so PNG compresses poorly and JPEG is smaller.
	src := image.NewNRGBA(image.Rect(0, 0, 200, 200))
	noise := rand.New(rand.NewPCG(1, 2))
	for y := range 200 {
		for x := range 200 {
			src.Set(x, y, color.NRGBA{R: uint8(noise.IntN(256)), G: uint8(noise.IntN(256)), B: uint8(noise.IntN(256)), A: 255})
		}
	}
	for y := range 32 { // fully transparent block
		for x := range 32 {
			src.Set(x, y, color.NRGBA{})
		}
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, src); err != nil {
		t.Fatal(err)
	}
	out := normalizeImage(pngData.Bytes())
	if http.DetectContentType(out) != "image/jpeg" || len(out) >= pngData.Len() {
		t.Fatalf("type %s, %d -> %d bytes", http.DetectContentType(out), pngData.Len(), len(out))
	}
	decoded, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil || decoded.Bounds().Dx() != 200 {
		t.Fatalf("decode: %v", err)
	}
	// Transparent pixels turn white, not black.
	if r, g, b, _ := decoded.At(8, 8).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("transparent pixel is %d,%d,%d", r>>8, g>>8, b>>8)
	}

	// Anything else passes through untouched.
	for _, in := range [][]byte{[]byte("not an image"), out, []byte("\x89PNG\r\n\x1a\nbroken")} {
		if got := normalizeImage(in); !bytes.Equal(got, in) {
			t.Fatalf("changed %q", in[:8])
		}
	}
	// A PNG that JPEG cannot shrink is kept: tinyPNG is 67 bytes.
	if got := normalizeImage(tinyPNG); !bytes.Equal(got, tinyPNG) {
		t.Fatal("tiny PNG was replaced by a larger JPEG")
	}
}

func TestOpenRouterGenerateSendsFreshSeeds(t *testing.T) {
	var mu sync.Mutex
	seeds := map[float64]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seeds[body["seed"].(float64)] = true
		mu.Unlock()
		_, _ = w.Write([]byte(imageReply(base64.StdEncoding.EncodeToString([]byte("x")))))
	}))
	defer srv.Close()
	o := &openRouter{key: "k", model: "m", base: srv.URL, client: srv.Client()}
	for range 5 {
		if _, err := o.Generate(context.Background(), "a cat"); err != nil {
			t.Fatal(err)
		}
	}
	if len(seeds) < 2 {
		t.Fatalf("seeds repeat: %v", seeds)
	}
}

func TestOpenRouterGenerateRetriesWithoutSeed(t *testing.T) {
	var calls []bool // whether each request carried a seed
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, hasSeed := body["seed"]
		calls = append(calls, hasSeed)
		if hasSeed {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Model does not support: seed.","code":400}}`))
			return
		}
		_, _ = w.Write([]byte(imageReply(base64.StdEncoding.EncodeToString([]byte("x")))))
	}))
	defer srv.Close()
	o := &openRouter{key: "k", model: "m", base: srv.URL, client: srv.Client()}
	if data, err := o.Generate(context.Background(), "a cat"); err != nil || string(data) != "x" {
		t.Fatalf("got %q, %v", data, err)
	}
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("calls with seed = %v, want [true false]", calls)
	}
}

func TestOpenRouterGenerateReportsUpstreamMessage(t *testing.T) {
	o, _ := fakeOpenRouter(t, 400, `{"error":{"message":"bad prompt shape"}}`)
	_, err := o.Generate(context.Background(), "x")
	if !errors.Is(err, errAIUpstream) || !strings.Contains(err.Error(), "bad prompt shape") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenRouterGenerateFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unauthorized", 401, `{}`, errAIUpstream},
		{"out of credit", 402, `{}`, errAIRateLimit},
		{"rate limited", 429, `{}`, errAIRateLimit},
		{"bad json", 200, `nope`, errAIUpstream},
		{"no data", 200, `{"data":[]}`, errAINoImage},
		{"empty image", 200, imageReply(""), errAINoImage},
		{"bad base64", 200, imageReply("!!!"), errAINoImage},
		{"too large", 200, imageReply(strings.Repeat("AAAA", maxImageBytes/3+1000)), errAIImageLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, _ := fakeOpenRouter(t, c.status, c.body)
			if _, err := o.Generate(context.Background(), "x"); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestNewOpenRouterNeedsKeyAndModel(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "k")
	t.Setenv("OPENROUTER_MODEL", "")
	if newOpenRouter() != nil {
		t.Fatal("expected nil without a model")
	}
	t.Setenv("OPENROUTER_MODEL", "m")
	if o := newOpenRouter(); o == nil || o.base != defaultOpenRouterBase {
		t.Fatalf("got %+v", o)
	}
}
