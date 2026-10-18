package assist

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"homeport/internal/sysload"
)

// The markup of Ollama's pages, cut down to what Homeport reads.
const searchPage = `<ul role="list">
<li class="border-b"><a href="/library/qwen3.5" class="group flex">
  <h2 title="qwen3.5"><span>qwen3.5</span></h2>
  <p class="mt-1" title="Qwen 3.5 is a family of open-source multimodal models &amp; more.">Qwen 3.5…</p>
  <div><span  class="inline-flex items-center rounded-md bg-indigo-50 px-2">vision</span><span  class="inline-flex rounded-md bg-indigo-50 px-2">tools</span>
  <span  class="inline-flex items-center rounded-md bg-[#ddf4ff] px-2">4b</span><span  class="inline-flex rounded-md bg-[#ddf4ff] px-2">35b</span></div>
  <span title="22,212,345 downloads"><span>22.2M</span></span></a></li>
<li class="border-b"><a href="/library/llama3.2" class="group w-full space-y-5"><div title="llama3.2"><h2><span>llama3.2</span></h2>
  <p class="max-w-lg break-words text-neutral-800 text-md">Meta's Llama 3.2 goes small with 1B and 3B models.</p></div>
  <span  class="inline-flex items-center rounded-md bg-indigo-50 px-2">tools</span><span  class="inline-flex rounded-md bg-[#ddf4ff] px-2">1b</span>
  <span class="flex items-center"><svg></svg>
                <span >58.1M</span>
                <span class="hidden sm:flex">&nbsp;Pulls</span></span></a></li>
<li class="border-b"><a href="/library/nomic-embed-text" class="group flex"><p title="Embeddings">x</p>
  <span class="inline-flex rounded-md bg-indigo-50 px-2">embedding</span><span class="inline-flex rounded-md bg-[#ddf4ff] px-2">v1.5</span></a></li>
<li class="border-b"><a href="/library/big-cloud" class="group flex"><p title="Only in the cloud">x</p>
  <span class="inline-flex rounded-md bg-indigo-50 px-2">tools</span></a></li>
</ul>`

func tagRow(model, size string) string {
	return fmt.Sprintf(`<div class="group px-4 py-3"><a href="/library/%s" class="md:hidden flex flex-col">
  <span class="group-hover:underline">%s</span><div class="flex flex-col text-neutral-500 text-[13px]"><span><span class="font-mono">abc</span> • %s • 256K context window  •
  </span></div></a><div class="hidden md:flex"><a href="/library/%s" class="group-hover:underline">x</a></div></div>`, model, model, size, model)
}

func fakeLibrary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/html" && !strings.HasPrefix(r.URL.Path, "/v2/") {
			http.Error(w, "wants html", 400)
			return
		}
		switch r.URL.Path {
		case "/search", "/library":
			fmt.Fprint(w, searchPage)
		case "/library/qwen3.5/tags":
			fmt.Fprint(w, tagRow("qwen3.5:latest", "6.6GB - 7.6GB")+tagRow("qwen3.5:4b", "3.3GB")+tagRow("qwen3.5:4b-q8_0", "5.1GB")+
				tagRow("qwen3.5:35b", "22GB")+tagRow("qwen3.5:35b-a3b", "22GB")+tagRow("qwen3.5:400b-cloud", "-"))
		case "/v2/library/qwen3.5/manifests/4b":
			fmt.Fprint(w, `{"layers":[{"mediaType":"application/vnd.ollama.image.projector","size":680000000},{"mediaType":"application/vnd.ollama.image.model","size":2650000000}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldLib, oldReg := libraryURL, registryURL
	libraryURL, registryURL = srv.URL, srv.URL
	libCache.Clear()
	t.Cleanup(func() { libraryURL, registryURL = oldLib, oldReg; libCache.Clear() })
}

func TestSearchLibrary(t *testing.T) {
	fakeLibrary(t)
	for _, q := range []string{"qwen", ""} {
		found, err := SearchLibrary(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != 2 {
			t.Fatalf("found %+v (embedding and cloud-only models should be left out)", found)
		}
		// The popular list's markup is a little different.
		if l := found[1]; l.Name != "llama3.2" || l.Description != "Meta's Llama 3.2 goes small with 1B and 3B models." || l.Downloads != "58.1M" {
			t.Errorf("got %+v", l)
		}
		m := found[0]
		if m.Name != "qwen3.5" || m.Description != "Qwen 3.5 is a family of open-source multimodal models & more." ||
			strings.Join(m.Caps, ",") != "vision,tools" || strings.Join(m.Sizes, ",") != "4b,35b" || m.Downloads != "22.2M" {
			t.Errorf("got %+v", m)
		}
	}
}

func TestLibraryVariants(t *testing.T) {
	fakeLibrary(t)
	vs, err := LibraryVariants(context.Background(), "qwen3.5")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range vs {
		got = append(got, fmt.Sprintf("%s %d %d %g/%g", v.Model, v.Download, v.Weights, v.Params, v.Active))
	}
	// latest and quantized builds left out, 35b and 35b-a3b are one download, cloud-only skipped; weights from the
	// registry (or the download's size when it can't tell).
	want := "qwen3.5:4b 3300000000 2650000000 4/0, qwen3.5:35b-a3b 22000000000 22000000000 35/3"
	if strings.Join(got, ", ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, ", "), want)
	}
}

func TestParamsOf(t *testing.T) {
	for model, want := range map[string]string{
		"qwen3.5:4b": "4/0", "qwen3.5:35b-a3b": "35/3", "gemma3:270m": "0.27/0", "gemma3n:e4b": "4/0",
		"gpt-oss:20b": "20/3.6", "llama3.2": "0/0", "qwen3.5:latest": "0/0",
	} {
		p, a := ParamsOf(model)
		if got := fmt.Sprintf("%g/%g", p, a); got != want {
			t.Errorf("%s: %s, want %s", model, got, want)
		}
	}
}

// The test server: an older 6-core Xeon (no AVX2), 32 GB, a graphics card Ollama can't use; qwen3.5:4b wrote 3.7
// tokens a second.
func TestEstimates(t *testing.T) {
	m := Machine{Hardware: sysload.Hardware{Memory: 32 << 30, Cores: 12, GPU: "Radeon HD 7870 XT", GPUKind: "amd", GPUMemory: 3 << 30}}
	m.Measure(3.7, 2_650_000_000, 4, 0)
	vs := []Variant{
		{Model: "qwen3.5:0.8b", Weights: 810_000_000, Params: 0.8},
		{Model: "qwen3.5:2b", Weights: 2_010_000_000, Params: 2},
		{Model: "qwen3.5:4b", Weights: 2_650_000_000, Params: 4},
		{Model: "qwen3.5:27b", Weights: 16_500_000_000, Params: 27},
		{Model: "qwen3.5:122b-a10b", Weights: 81_400_000_000, Params: 122, Active: 10},
	}
	for i := range vs {
		e := m.Estimate(vs[i].Weights, vs[i].Params, vs[i].Active)
		vs[i].Estimate = &e
	}
	PickBest(vs)
	var got []string
	for _, v := range vs {
		got = append(got, fmt.Sprintf("%s %s %.1f %s %v", v.Model, v.Estimate.Label, v.Estimate.Words, v.Estimate.Place, v.Best))
	}
	want := []string{
		"qwen3.5:0.8b Good 9.1 processor false",
		"qwen3.5:2b Usable 3.7 processor true", // the biggest at a usable speed
		"qwen3.5:4b Slow 2.8 processor false",  // as measured: 3.7 tokens ≈ 2.8 words a second
		"qwen3.5:27b Slow 0.4 processor false",
		"qwen3.5:122b-a10b Too big 0.0  false",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !vs[2].Estimate.Measured || vs[0].Estimate.Measured != true {
		t.Error("estimates on the processor should use the measured speed")
	}

	// An NVIDIA card with 12 GB: what fits on it is fast, what doesn't runs on both.
	g := Machine{Hardware: sysload.Hardware{Memory: 32 << 30, AVX2: true, GPU: "RTX 3060", GPUKind: "nvidia", GPUMemory: 12 << 30}, GPUWorks: true}
	if e := g.Estimate(5_630_000_000, 9, 0); e.Place != "graphics card" || e.Label != "Fast" {
		t.Errorf("9b on a 3060: %+v", e)
	}
	if e := g.Estimate(16_500_000_000, 27, 0); e.Place != "graphics card and processor" {
		t.Errorf("27b on a 3060: %+v", e)
	}
	// A Mac with 16 GB shares its memory with the graphics.
	mac := Machine{Hardware: sysload.Hardware{Memory: 16 << 30, Apple: true}}
	if e := mac.Estimate(5_630_000_000, 9, 0); e.Place != "graphics" || e.Label != "Good" {
		t.Errorf("9b on a 16 GB Mac: %+v", e)
	}
	if e := mac.Estimate(16_500_000_000, 27, 0); e.Label != "Too big" {
		t.Errorf("27b on a 16 GB Mac: %+v", e)
	}
}

func TestOllamaPull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.RawQuery, "missing") {
			return
		}
		for _, l := range []string{
			`{"status":"pulling manifest"}`,
			`{"status":"pulling aaa","digest":"sha256:aaa","total":1000,"completed":500}`,
			`{"status":"pulling bbb","digest":"sha256:bbb","total":3000,"completed":0}`,
			`{"status":"pulling aaa","digest":"sha256:aaa","total":1000,"completed":1000}`,
			`{"status":"pulling bbb","digest":"sha256:bbb","total":3000,"completed":3000}`,
			`{"status":"verifying sha256 digest"}`,
			`{"status":"success"}`,
		} {
			fmt.Fprintln(w, l)
		}
	}))
	defer srv.Close()
	var seen []string
	err := OllamaPull(context.Background(), srv.URL, "qwen3.5:4b", func(p PullProgress) {
		seen = append(seen, fmt.Sprintf("%s %d/%d", p.Status, p.Done, p.Total))
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "pulling manifest 0/0, downloading 500/1000, downloading 500/4000, downloading 1000/4000, downloading 4000/4000, verifying sha256 digest 4000/4000, success 4000/4000"
	if strings.Join(seen, ", ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(seen, ", "), want)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"pulling manifest"}`)
		fmt.Fprintln(w, `{"error":"pull model manifest: file does not exist"}`)
	}))
	defer bad.Close()
	if err := OllamaPull(context.Background(), bad.URL, "nope:1b", nil); err == nil || !strings.Contains(err.Error(), "no model by that name") {
		t.Errorf("err = %v", err)
	}
}

func TestIntelArcRunsModels(t *testing.T) {
	m := Machine{Hardware: sysload.Hardware{Memory: 16 << 30, AVX2: true, GPU: "Intel Arc B580", GPUKind: "intel", GPUMemory: 12 << 30}, GPUWorks: true}
	if e := m.Estimate(6<<30, 9, 0); e.Place != "graphics card" {
		t.Fatalf("a 6 GB model fits a 12 GB Arc card: %+v", e)
	}
	if e := m.Estimate(20<<30, 26, 4); e.Place != "graphics card and processor" {
		t.Fatalf("a 20 GB model is split between the card and the processor: %+v", e)
	}
}
