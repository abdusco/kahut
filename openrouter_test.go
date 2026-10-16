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
	out := pngToJPEG(pngData.Bytes())
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
		if got := pngToJPEG(in); !bytes.Equal(got, in) {
			t.Fatalf("changed %q", in[:8])
		}
	}
	// A PNG that JPEG cannot shrink is kept: tinyPNG is 67 bytes.
	if got := pngToJPEG(tinyPNG); !bytes.Equal(got, tinyPNG) {
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
