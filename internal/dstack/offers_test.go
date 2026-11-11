package dstack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"billing-backend/internal/domain"
)

func TestOffersAskForOffersOnlyAndNeverReserveCapacity(t *testing.T) {
	var forwarded map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &forwarded)
		if r.URL.Path != "/api/project/tenant-project/runs/get_plan" {
			t.Errorf("forwarded path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job_plans":[{"offers":[
			{"backend":"vastai","region":"us-utah","price":0.29,"availability":"available","instance":{"name":"47510354","resources":{"gpus":[{"name":"RTX 3090","memory_mib":24576}]}}},
			{"backend":"runpod","region":"CA-MTL-1","price":0.27,"availability":"available","instance":{"name":"NVIDIA RTX A5000","resources":{"gpus":[{"name":"A5000","memory_mib":24576}]}}}
		]}]}`))
	}))
	defer server.Close()
	proxy := NewProxy(server.URL, proxyCredential("tenant-token"), nil, nil)
	proxy.HTTP = server.Client()
	backends, err := ParseProviderBackends([]byte("backends:\n  - type: runpod\n  - type: vastai\n"))
	if err != nil {
		t.Fatal(err)
	}
	proxy.Backends = backends
	binding := domain.DStackBinding{Project: "tenant-project", Username: "tenant-user", Provision: domain.ProvisionReady}
	offers, err := proxy.Offers(context.Background(), "tenant-id", binding, 1, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 2 {
		t.Fatalf("offers = %+v", offers)
	}
	if offers[0].Provider != "runpod" || offers[0].PricePerHour != 0.27 {
		t.Errorf("cheapest offer = %+v, want runpod first", offers[0])
	}
	if offers[0].GPUMemoryGB != 24 || offers[0].GPUCount != 1 || offers[0].GPUName != "A5000" {
		t.Errorf("offer detail = %+v", offers[0])
	}
	if forwarded["for_offers_only"] != true {
		t.Errorf("request did not ask for offers only: %v", forwarded["for_offers_only"])
	}
	runSpec, _ := forwarded["run_spec"].(map[string]any)
	configuration, _ := runSpec["configuration"].(map[string]any)
	resources, _ := configuration["resources"].(map[string]any)
	gpu, _ := resources["gpu"].(map[string]any)
	if gpu["count"] != float64(1) || gpu["memory"] != "24GB" {
		t.Errorf("gpu requirements = %v", gpu)
	}
	if volumes, exists := configuration["volumes"]; exists {
		t.Errorf("offer request carried volumes: %v", volumes)
	}
}

func TestOffersRejectUnboundedRequests(t *testing.T) {
	proxy := NewProxy("https://dstack.invalid", proxyCredential("tenant-token"), nil, nil)
	binding := domain.DStackBinding{Project: "tenant-project", Username: "tenant-user", Provision: domain.ProvisionReady}
	for _, test := range []struct {
		count   int
		memory  float64
		wantErr string
	}{
		{0, 0, "gpu_count"},
		{9, 0, "gpu_count"},
		{1, -1, "min_memory_gb"},
		{1, 2048, "min_memory_gb"},
	} {
		if _, err := proxy.Offers(context.Background(), "tenant-id", binding, test.count, test.memory); err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("count=%d memory=%v error = %v, want %q", test.count, test.memory, err, test.wantErr)
		}
	}
}
