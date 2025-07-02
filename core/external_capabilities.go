package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"sync"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/golang/glog"
)

type ExternalCapability struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Url           string `json:"url"`
	Capacity      int    `json:"capacity"`
	PricePerUnit  int64  `json:"price_per_unit"`
	PriceScaling  int64  `json:"price_scaling"`
	PriceCurrency string `json:"currency"`

	price *AutoConvertedPrice

	mu   sync.RWMutex
	Load int
}

type ExternalCapabilities struct {
	capm         sync.Mutex
	Capabilities map[string]*ExternalCapability
	Streams      map[string]*StreamData
}

type StreamData struct {
	StreamID   string
	Capability string
	//Gateway fields
	OrchUrl      string
	ExcludeOrchs []string

	//Orchestrator fields
	Sender ethcommon.Address
	//source stream
	StreamCtx         context.Context
	CancelStream      context.CancelFunc
	StreamRelayServer interface{}
	streamActiveTime  time.Time
	//result stream
	WorkerStreamCtx    context.Context
	WorkerCancelStream context.CancelFunc
	WorkerRelayServer  interface{}
}

func NewExternalCapabilities() *ExternalCapabilities {
	extCaps := &ExternalCapabilities{
		Capabilities: make(map[string]*ExternalCapability),
		Streams:      make(map[string]*StreamData),
	}

	// Start a ticker to run stream monitoring every minute
	go extCaps.StartStreamMonitor()

	return extCaps
}

func (extCaps *ExternalCapabilities) StartStreamMonitor() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			glog.Infof("Running stream monitor at %s", time.Now().Format(time.RFC3339))
		}
	}
}

func (extCaps *ExternalCapabilities) StopStream(streamID string) error {
	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()

	streamData, exists := extCaps.Streams[streamID]
	if !exists {
		return fmt.Errorf("stream %s does not exist", streamID)
	}

	//if on Orchestrator, need to stop the relay servers
	if streamData.CancelStream != nil {
		streamData.CancelStream()
		streamData.StreamRelayServer = nil
	}
	if streamData.WorkerCancelStream != nil {
		streamData.WorkerCancelStream()
		streamData.WorkerRelayServer = nil
	}

	delete(extCaps.Streams, streamID)
	glog.V(4).Infof("Stopped and removed stream %s", streamID)

	return nil
}

func (extCaps *ExternalCapabilities) RemoveCapability(extCap string) {
	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()

	delete(extCaps.Capabilities, extCap)
}

func (extCaps *ExternalCapabilities) RegisterCapability(extCapability string) (*ExternalCapability, error) {
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
	if cap, ok := extCaps.Capabilities[extCap.Name]; ok {
		cap.Url = extCap.Url
		cap.Capacity = extCap.Capacity
		cap.price = extCap.price
	}

	extCaps.Capabilities[extCap.Name] = &extCap

	return &extCap, err
}

func (extCap *ExternalCapability) GetPrice() *big.Rat {
	extCap.mu.RLock()
	defer extCap.mu.RUnlock()
	return extCap.price.Value()
}
