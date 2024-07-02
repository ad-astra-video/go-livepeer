package core

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/livepeer/ai-worker/worker"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/net"
	"github.com/stretchr/testify/assert"
)

type StubAIWorkerServer struct {
	manager         *RemoteAIWorkerManager
	stream          *net.AIWorker_RegisterAIWorkerServer
	SendError       error
	WorkerError     error
	WithholdResults bool

	common.StubServerStream
}

func (s *StubAIWorkerServer) Send(n *net.NotifySegment) error {
	var images []worker.Media
	images = append(images, worker.Media{Url: "file1", Nsfw: false, Seed: 1})
	files := make(map[string][]byte)
	files["file1"] = []byte("image")
	res := RemoteAIWorkerResult{
		Results: &worker.ImageResponse{Images: images},
		Files:   files,
		Err:     s.WorkerError,
	}

	if !s.WithholdResults {
		s.manager.aiResults(n.TaskId, &res)
	}

	return s.SendError
}

func TestServeAIWorker(t *testing.T) {
	n, _ := NewLivepeerNode(nil, "", nil)
	n.AIWorkerManager = NewRemoteAIWorkerManager()
	srv := &StubAIWorkerServer{}

	// test that a RemoteAIWorker was created
	netCaps := aiWorkerCapabilitiesWithConstraints()

	go n.serveAIWorker(*srv.stream, netCaps)
	time.Sleep(1 * time.Second)

	rw, ok := n.AIWorkerManager.liveAIWorkers[*srv.stream]
	if !ok {
		t.Error("Unexpected ai worker type")
	}

	// test shutdown
	rw.eof <- struct{}{}
	time.Sleep(1 * time.Second)

	// stream should be removed
	_, ok = n.AIWorkerManager.liveAIWorkers[*srv.stream]
	if ok {
		t.Error("Unexpected aiworker presence")
	}
}

func TestRemoteAIWorker(t *testing.T) {
	m := NewRemoteAIWorkerManager()

	initAIWorker := func() (*RemoteAIWorker, *StubAIWorkerServer) {
		srv := &StubAIWorkerServer{manager: m}
		rw := createRemoteAIWorker(m, *srv.stream)
		return rw, srv
	}

	// happy path
	rw, srv := initAIWorker()
	req := make(map[string]string)
	req["pipeline"] = "image-to-image"
	req["model_id"] = "model"
	req["prompt"] = "a new exciting test prompt"
	res, err := rw.Process(context.TODO(), "image-to-image", "model", "file1", req)

	if err != nil || res.Results.Images[0].Seed != 1 {
		t.Error("error processing ai worker request ", err)
	}

	// error on remote while processing
	rw, srv = initAIWorker()
	srv.WorkerError = fmt.Errorf("ProcessError")
	res, err = rw.Process(context.TODO(), "image-to-image", "model", "file1", req)
	if err != srv.WorkerError {
		t.Error("Unexpected error ", err, res)
	}

	// simulate error with sending
	rw, srv = initAIWorker()

	srv.SendError = fmt.Errorf("SendError")
	res, err = rw.Process(context.TODO(), "image-to-image", "model", "file1", req)
	if _, fatal := err.(RemoteAIWorkerFatalError); !fatal ||
		err.Error() != srv.SendError.Error() {
		t.Error("Unexpected error ", err, fatal)
	}

	assert := assert.New(t)

	// check default timeout
	rw, srv = initAIWorker()
	srv.WithholdResults = true
	m.taskCount = 1001
	oldTimeout := common.HTTPTimeout
	defer func() { common.HTTPTimeout = oldTimeout }()
	common.HTTPTimeout = 5 * time.Millisecond

	// count relative ticks rather than wall clock to mitigate CI slowness
	countTicks := func(exitVal chan int, stopper chan struct{}) {
		ticker := time.NewTicker(time.Millisecond)
		ticks := 0
		for {
			select {
			case <-stopper:
				exitVal <- ticks
				return
			case <-ticker.C:
				ticks++
			}
		}
	}
	tickCh := make(chan int, 1)
	stopper := make(chan struct{})
	go countTicks(tickCh, stopper)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		res, err = rw.Process(context.TODO(), "image-to-image", "model", "file1", req)
		assert.Equal("Remote worker took too long", err.Error())
		wg.Done()
	}()
	assert.True(wgWait(&wg), "transcoder took too long to timeout")
	stopper <- struct{}{}
}

func TestManageAIWorkers(t *testing.T) {
	m := NewRemoteAIWorkerManager()
	srv := &StubAIWorkerServer{}
	srv2 := &StubAIWorkerServer{manager: m}

	// sanity check that liveAIWorkers and remoteAIWorkers is empty
	assert := assert.New(t)
	assert.Nil(m.liveAIWorkers[*srv.stream])
	assert.Nil(m.liveAIWorkers[*srv2.stream])
	assert.Empty(m.remoteAIWorkers)
	assert.Equal(0, m.RegisteredAIWorkersCount())

	netCaps := aiWorkerCapabilitiesWithConstraints()

	// test that worker is added to liveAIWorkers and remoteAIWorkers
	wg1 := newWg(1)
	go func() { m.Manage(*srv.stream, netCaps); wg1.Done() }()
	time.Sleep(1 * time.Millisecond) // allow the manager to activate

	assert.NotNil(m.liveAIWorkers[*srv.stream])
	assert.Len(m.liveAIWorkers, 1)
	assert.Len(m.remoteAIWorkers, 1)
	assert.Equal(1, m.RegisteredAIWorkersCount())
	rwi := m.RegisteredAIWorkersInfo()
	assert.Len(rwi, 1)
	assert.Equal(1, rwi[0].Capabilities["Image to image"]["model"].Capacity)
	assert.Equal("TestAddress", rwi[0].Address)

	// test that additional transcoder is added to liveTranscoders and remoteTranscoders
	wg2 := newWg(1)
	go func() { m.Manage(*srv2.stream, netCaps); wg2.Done() }()
	time.Sleep(1 * time.Millisecond) // allow the manager to activate

	assert.NotNil(m.liveAIWorkers[*srv.stream])
	assert.NotNil(m.liveAIWorkers[*srv2.stream])
	assert.Len(m.liveAIWorkers, 2)
	assert.Len(m.remoteAIWorkers, 2)
	assert.Equal(2, m.RegisteredAIWorkersCount())

	// test that workers are removed from liveAIWorkers and remoteAIWorkers
	m.liveAIWorkers[*srv.stream].eof <- struct{}{}
	assert.True(wgWait(wg1)) // time limit
	assert.Nil(m.liveAIWorkers[*srv.stream])
	assert.NotNil(m.liveAIWorkers[*srv2.stream])
	assert.Len(m.liveAIWorkers, 1)
	assert.Len(m.remoteAIWorkers, 2)
	assert.Equal(1, m.RegisteredAIWorkersCount())

	m.liveAIWorkers[*srv2.stream].eof <- struct{}{}
	assert.True(wgWait(wg2)) // time limit
	assert.Nil(m.liveAIWorkers[*srv.stream])
	assert.Nil(m.liveAIWorkers[*srv.stream])
	assert.Len(m.liveAIWorkers, 0)
	assert.Len(m.remoteAIWorkers, 2)
	assert.Equal(0, m.RegisteredAIWorkersCount())
}

func aiWorkerCapabilitiesWithConstraints() *net.Capabilities {
	var aiCaps []Capability
	constraints := make(map[Capability]*Constraints)
	aiCaps = append(aiCaps, Capability_ImageToImage)
	aiCaps = append(aiCaps, DefaultCapabilities()...) //need to have default Capabilities, not discoverable by Gateway without
	constraints[Capability_ImageToImage] = &Constraints{
		Models: make(map[string]*ModelConstraint),
	}
	constraints[Capability_ImageToImage].Models["model"] = &ModelConstraint{Warm: false, Capacity: 1}

	capabilities := NewCapabilitiesWithConstraints(aiCaps, MandatoryOCapabilities(), constraints)
	return capabilities.ToNetCapabilities()
}

func createRemoteAIWorker(m *RemoteAIWorkerManager, strm net.AIWorker_RegisterAIWorkerServer) *RemoteAIWorker {
	aiWorkerNetCaps := aiWorkerCapabilitiesWithConstraints()
	return NewRemoteAIWorker(m, strm, aiWorkerNetCaps.Constraints, CapabilitiesFromNetCapabilities(aiWorkerNetCaps))
}
