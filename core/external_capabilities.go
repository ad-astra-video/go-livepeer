package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"sort"
	"time"

	"sync"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/monitor"
	"github.com/livepeer/go-livepeer/trickle"
)

type ExternalCapability struct {
	ID            string `json:"id,omitempty"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Url           string `json:"url"`
	Order         int    `json:"order,omitempty"`
	Capacity      int    `json:"capacity"`
	PricePerUnit  int64  `json:"price_per_unit"`
	PriceScaling  int64  `json:"price_scaling"`
	PriceCurrency string `json:"currency"`
	AuthToken     string `json:"token"`
	Key           string `json:"key,omitempty"`

	price *AutoConvertedPrice

	Mu   sync.RWMutex
	Load int
}

type StreamInfo struct {
	StreamID   string
	Capability string
	RunnerKey  string
	WorkerURL  string

	//Orchestrator fields
	Sender         ethcommon.Address
	StreamRequest  []byte
	pubChannel     *trickle.TrickleLocalPublisher
	subChannel     *trickle.TrickleLocalPublisher
	controlChannel *trickle.TrickleLocalPublisher
	eventsChannel  *trickle.TrickleLocalPublisher
	dataChannel    *trickle.TrickleLocalPublisher
	//Stream fields
	JobParams    string
	StreamCtx    context.Context
	CancelStream context.CancelFunc

	cleanupOnce sync.Once
	sdm         sync.Mutex
}

func (sd *StreamInfo) IsActive() bool {
	sd.sdm.Lock()
	defer sd.sdm.Unlock()
	if sd.StreamCtx.Err() != nil {
		return false
	}

	if sd.controlChannel == nil {
		return false
	}

	return true
}

func (sd *StreamInfo) UpdateParams(params string) {
	sd.sdm.Lock()
	defer sd.sdm.Unlock()
	sd.JobParams = params
}

func (sd *StreamInfo) SetChannels(pub, sub, control, events, data *trickle.TrickleLocalPublisher) {
	sd.sdm.Lock()
	defer sd.sdm.Unlock()
	sd.pubChannel = pub
	sd.subChannel = sub
	sd.controlChannel = control
	sd.eventsChannel = events
	sd.dataChannel = data
}

func (sd *StreamInfo) SetRunner(runnerKey, workerURL string) {
	sd.sdm.Lock()
	defer sd.sdm.Unlock()
	sd.RunnerKey = runnerKey
	sd.WorkerURL = workerURL
}

func (sd *StreamInfo) cleanup() {
	sd.cleanupOnce.Do(func() {
		// Close all channels exactly once
		if sd.pubChannel != nil {
			sd.pubChannel.Close()
		}
		if sd.subChannel != nil {
			sd.subChannel.Close()
		}
		if sd.controlChannel != nil {
			sd.controlChannel.Close()
		}
		if sd.eventsChannel != nil {
			sd.eventsChannel.Close()
		}
		if sd.dataChannel != nil {
			sd.dataChannel.Close()
		}
	})
}

type ExternalCapabilities struct {
	capm         sync.Mutex
	Capabilities map[string]*ExternalCapability
	Streams      map[string]*StreamInfo
}

func NewExternalCapabilities() *ExternalCapabilities {
	return &ExternalCapabilities{
		Capabilities: make(map[string]*ExternalCapability),
		Streams:      make(map[string]*StreamInfo)}
}

func (extCaps *ExternalCapabilities) AddStream(streamID string, capability string, streamReq []byte) (*StreamInfo, error) {
	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()
	_, ok := extCaps.Streams[streamID]
	if ok {
		return nil, fmt.Errorf("stream already exists: %s", streamID)
	}

	//add to streams
	ctx, cancel := context.WithCancel(context.Background())
	stream := StreamInfo{
		StreamID:      streamID,
		Capability:    capability,
		StreamRequest: streamReq,
		StreamCtx:     ctx,
		CancelStream:  cancel,
	}
	extCaps.Streams[streamID] = &stream

	//clean up when stream ends
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		defer stream.cleanup()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Periodically check if stream still exists in map
				extCaps.capm.Lock()
				_, exists := extCaps.Streams[streamID]
				extCaps.capm.Unlock()
				if !exists {
					return
				}
			}
		}
	}()

	return &stream, nil
}

func (extCaps *ExternalCapabilities) RemoveStream(streamID string) {
	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()

	streamInfo, ok := extCaps.Streams[streamID]
	if ok {
		//confirm stream context is canceled before deleting
		if streamInfo.StreamCtx.Err() == nil {
			streamInfo.CancelStream()
		}
	}

	delete(extCaps.Streams, streamID)
}

func (extCaps *ExternalCapabilities) GetStream(streamID string) (*StreamInfo, bool) {
	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()

	streamInfo, ok := extCaps.Streams[streamID]
	return streamInfo, ok
}

func (extCaps *ExternalCapabilities) StreamExists(streamID string) bool {
	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()

	_, ok := extCaps.Streams[streamID]
	return ok
}

func (extCaps *ExternalCapabilities) RemoveCapability(extCap string) {
	if extCaps == nil {
		return
	}

	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()

	if _, ok := extCaps.Capabilities[extCap]; ok {
		delete(extCaps.Capabilities, extCap)
		return
	}

	for key, cap := range extCaps.Capabilities {
		if cap.Name == extCap {
			delete(extCaps.Capabilities, key)
		}
	}
}

func (extCaps *ExternalCapabilities) RegisterCapability(extCapability string) (*ExternalCapability, error) {
	if extCaps == nil {
		return nil, fmt.Errorf("external capabilities not initialized")
	}

	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()
	if extCaps.Capabilities == nil {
		extCaps.Capabilities = make(map[string]*ExternalCapability)
	}
	var extCap ExternalCapability
	err := json.Unmarshal([]byte(extCapability), &extCap)
	if err != nil {
		return nil, err
	}

	key, err := extCap.registrationKey()
	if err != nil {
		return nil, err
	}
	extCap.Key = key

	//ensure PriceScaling is not 0
	if extCap.PriceScaling == 0 {
		extCap.PriceScaling = 1
	}
	extCap.price, err = NewAutoConvertedPrice(extCap.PriceCurrency, big.NewRat(extCap.PricePerUnit, extCap.PriceScaling), func(price *big.Rat) {
		glog.V(6).Infof("Capability %s price set to %s wei per compute unit", extCap.Name, price.FloatString(3))
	})

	if err != nil {
		panic(fmt.Errorf("error converting price: %v", err))
	}
	if cap, ok := extCaps.Capabilities[key]; ok {
		cap.ID = extCap.ID
		cap.Name = extCap.Name
		cap.Description = extCap.Description
		cap.Url = extCap.Url
		cap.Order = extCap.Order
		cap.Capacity = extCap.Capacity
		cap.PricePerUnit = extCap.PricePerUnit
		cap.PriceScaling = extCap.PriceScaling
		cap.PriceCurrency = extCap.PriceCurrency
		cap.price = extCap.price
		cap.AuthToken = extCap.AuthToken
		cap.Key = key
		return cap, nil
	}

	extCaps.Capabilities[key] = &extCap

	return &extCap, err
}

func (extCaps *ExternalCapabilities) GetCapabilityByKey(key string) (*ExternalCapability, bool) {
	if extCaps == nil {
		return nil, false
	}

	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()
	cap, ok := extCaps.Capabilities[key]
	return cap, ok
}

func (extCaps *ExternalCapabilities) GetCapabilitiesByName(name string) []*ExternalCapability {
	if extCaps == nil {
		return nil
	}

	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()
	return extCaps.capabilitiesByName(name)
}

func (extCaps *ExternalCapabilities) GetCapabilityByName(name string) (*ExternalCapability, bool) {
	if extCaps == nil {
		return nil, false
	}

	caps := extCaps.GetCapabilitiesByName(name)
	if len(caps) == 0 {
		return nil, false
	}
	return caps[0], true
}

func (extCaps *ExternalCapabilities) ReserveCapability(name string) (*ExternalCapability, error) {
	if extCaps == nil {
		return nil, fmt.Errorf("external capability not found")
	}

	extCaps.capm.Lock()

	for _, cap := range extCaps.capabilitiesByName(name) {
		cap.Mu.Lock()
		if cap.Load < cap.Capacity {
			cap.Load++
			currentLoad := cap.Load
			capabilityName := cap.Name
			runnerKey := cap.Key
			cap.Mu.Unlock()
			extCaps.capm.Unlock()
			monitor.AIRunnerAllocationsInFlight(capabilityName, runnerKey, int64(currentLoad))
			return cap, nil
		}
		cap.Mu.Unlock()
	}
	extCaps.capm.Unlock()

	return nil, fmt.Errorf("external capability not found")
}

func (extCaps *ExternalCapabilities) FreeCapability(key string) error {
	if extCaps == nil {
		return fmt.Errorf("external capability not found")
	}

	extCaps.capm.Lock()

	if cap, ok := extCaps.Capabilities[key]; ok {
		cap.Mu.Lock()
		if cap.Load > 0 {
			cap.Load--
		}
		currentLoad := cap.Load
		capabilityName := cap.Name
		runnerKey := cap.Key
		cap.Mu.Unlock()
		extCaps.capm.Unlock()
		monitor.AIRunnerAllocationsInFlight(capabilityName, runnerKey, int64(currentLoad))
		return nil
	}

	for _, cap := range extCaps.capabilitiesByName(key) {
		cap.Mu.Lock()
		if cap.Load > 0 {
			cap.Load--
			currentLoad := cap.Load
			capabilityName := cap.Name
			runnerKey := cap.Key
			cap.Mu.Unlock()
			extCaps.capm.Unlock()
			monitor.AIRunnerAllocationsInFlight(capabilityName, runnerKey, int64(currentLoad))
			return nil
		}
		cap.Mu.Unlock()
	}
	extCaps.capm.Unlock()

	return fmt.Errorf("external capability not found")
}

func (extCaps *ExternalCapabilities) AvailableCapacity(name string) int64 {
	if extCaps == nil {
		return 0
	}

	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()

	var total int64
	for _, cap := range extCaps.capabilitiesByName(name) {
		cap.Mu.RLock()
		remaining := cap.Capacity - cap.Load
		cap.Mu.RUnlock()
		if remaining > 0 {
			total += int64(remaining)
		}
	}

	return total
}

// caller should hold the extCaps.capm.Lock()
func (extCaps *ExternalCapabilities) capabilitiesByName(name string) []*ExternalCapability {
	var caps []*ExternalCapability
	for _, cap := range extCaps.Capabilities {
		if cap.Name == name {
			caps = append(caps, cap)
		}
	}

	sort.Slice(caps, func(i, j int) bool {
		if caps[i].Order != caps[j].Order {
			return caps[i].Order < caps[j].Order
		}
		return caps[i].Key < caps[j].Key
	})

	return caps
}

func (extCap *ExternalCapability) registrationKey() (string, error) {
	if extCap.ID != "" {
		return extCap.ID, nil
	}

	parsedURL, err := url.Parse(extCap.Url)
	if err != nil {
		return "", err
	}
	if parsedURL.Host == "" {
		return "", fmt.Errorf("runner url must include host:port")
	}
	return "runner_" + string(RandomManifestID()), nil
}

func (extCap *ExternalCapability) GetPrice() *big.Rat {
	extCap.Mu.RLock()
	defer extCap.Mu.RUnlock()
	return extCap.price.Value()
}
