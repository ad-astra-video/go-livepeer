package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/livepeer/ai-worker/worker"
	"github.com/livepeer/go-livepeer/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAIHTTP_RegisterWorker(t *testing.T) {
	assert := assert.New(t)
	//setup orchestrator and node with
	n, _ := core.NewLivepeerNode(nil, "", nil)
	n.OrchSecret = "super-secret"
	n.AIWorker, _ = worker.NewWorker("", nil, "/path/to/models", true)
	var l lphttp
	l.orchestrator = newStubOrchestrator()
	l.node = n

	worker1, worker1URL := stubExternalWorker(t, 200, "OK")
	defer worker1.Close()
	worker2, worker2URL := stubExternalWorker(t, 200, "OK")
	defer worker2.Close()

	var w = httptest.NewRecorder()

	startConfig := fmt.Sprintf(`[{"pipeline": "text-to-image","model_id": "ByteDance/SDXL-Lightning","url": "%v","price_per_unit": 0,
									"warm": true}]`, worker1URL.String())

	aiConfigs, err := core.ParseAIModelConfigs(startConfig)
	assert.Nil(err)
	aiCaps, aiConstraints, err := n.AddAIConfigs(context.TODO(), aiConfigs)
	assert.Nil(err)
	n.AddAICapabilities(context.TODO(), aiCaps, aiConstraints)

	newWorkerConfig := fmt.Sprintf(`[{"pipeline": "text-to-image","model_id": "ByteDance/SDXL-Lightning","url": "%v","price_per_unit": 0,
										"warm": true}]`, worker2URL.String())

	r, err := http.NewRequest(http.MethodPost, "/register-ai-worker", bytes.NewBufferString(newWorkerConfig))
	assert.NoError(err)

	r.Header.Set("Credentials", "super-secret")

	handler := l.RegisterAIWorker()
	handler.ServeHTTP(w, r)
	assert.Equal(http.StatusOK, w.Code)

	//check that worker2 caps were added, Capability_extToImage = 27
	caps := n.Capabilities.ToNetCapabilities()
	assert.Equal(caps.Constraints[27].Models["ByteDance/SDXL-Lightning"].Capacity, 2)

}

func TestAIHTTP_RegisterWorker_NoInitialConstraints(t *testing.T) {
	assert := assert.New(t)
	//setup orchestrator and node with
	n, _ := core.NewLivepeerNode(nil, "", nil)
	n.OrchSecret = "super-secret"
	n.AIWorker, _ = worker.NewWorker("", nil, "/path/to/models", true)
	var l lphttp
	l.orchestrator = newStubOrchestrator()
	l.node = n

	worker1, worker1URL := stubExternalWorker(t, 200, "OK")
	defer worker1.Close()
	workerConfig := fmt.Sprintf(`[{"pipeline": "text-to-image","model_id": "ByteDance/SDXL-Lightning","url": "%v","price_per_unit": 0,
									"warm": true}]`, worker1URL.String())

	var w = httptest.NewRecorder()

	r, err := http.NewRequest(http.MethodPost, "/register-ai-worker", bytes.NewBufferString(workerConfig))
	assert.NoError(err)

	r.Header.Set("Credentials", "super-secret")

	handler := l.RegisterAIWorker()
	handler.ServeHTTP(w, r)
	assert.Equal(http.StatusUnauthorized, w.Code)

	//check that worker2 caps were added, Capability_extToImage = 27
	caps := n.Capabilities.ToNetCapabilities()
	assert.Equal(caps.Constraints[27].Models["ByteDance/SDXL-Lightning"].Capacity, 1)

}

func TestAIHTTP_RegisterWorker_BadCredentials(t *testing.T) {
	assert := assert.New(t)
	//setup orchestrator and node with
	n, _ := core.NewLivepeerNode(nil, "", nil)
	n.OrchSecret = "super-secret"
	n.AIWorker, _ = worker.NewWorker("", nil, "/path/to/models", true)
	var l lphttp
	l.orchestrator = newStubOrchestrator()
	l.node = n

	var w = httptest.NewRecorder()
	r, err := http.NewRequest(http.MethodPost, "/register-ai-worker", bytes.NewBufferString(""))
	assert.NoError(err)

	r.Header.Set("Credentials", "not-super-secret")

	handler := l.RegisterAIWorker()
	handler.ServeHTTP(w, r)
	assert.Equal(http.StatusUnauthorized, w.Code)

}

func stubExternalWorker(t *testing.T, respCode int, respBody string) (*httptest.Server, *url.URL) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(respCode)
				if len(respBody) > 0 {
					fmt.Fprintln(w, respBody)
				}
			},
		),
	)

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	return server, serverURL
}
