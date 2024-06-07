package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"slices"

	"github.com/golang/glog"

	"github.com/livepeer/ai-worker/worker"
	"github.com/livepeer/go-livepeer/net"
)

type AI interface {
	TextToImage(context.Context, worker.TextToImageJSONRequestBody) (*worker.ImageResponse, error)
	ImageToImage(context.Context, worker.ImageToImageMultipartRequestBody) (*worker.ImageResponse, error)
	ImageToVideo(context.Context, worker.ImageToVideoMultipartRequestBody) (*worker.VideoResponse, error)
	Warm(context.Context, string, string, worker.RunnerEndpoint, worker.OptimizationFlags) error
	Stop(context.Context) error
	HasCapacity(pipeline, modelID string) bool
	IsRegistered(url, pipeline, modelID string) bool
}

type AIModelConfig struct {
	Pipeline          string                   `json:"pipeline"`
	ModelID           string                   `json:"model_id"`
	URL               string                   `json:"url,omitempty"`
	Token             string                   `json:"token,omitempty"`
	Warm              bool                     `json:"warm,omitempty"`
	PricePerUnit      int64                    `json:"price_per_unit,omitempty"`
	PixelsPerUnit     int64                    `json:"pixels_per_unit,omitempty"`
	OptimizationFlags worker.OptimizationFlags `json:"optimization_flags,omitempty"`
}

func PipelineToCapability(pipeline string) Capability {
	switch pipeline {
	case "text-to-image":
		return Capability_TextToImage
	case "image-to-image":
		return Capability_ImageToImage
	case "image-to-video":
		return Capability_ImageToVideo
	case "upscale":
		return Capability_Unused
	case "frame-interpolation":
		return Capability_Unused
	case "speech-to-text":
		return Capability_Unused
	default:
		return Capability_Unused
	}
}

func (config *AIModelConfig) UnmarshalJSON(data []byte) error {
	// Custom type to avoid recursive calls to UnmarshalJSON
	type AIModelConfigAlias AIModelConfig
	// Set default values for fields
	defaultConfig := &AIModelConfigAlias{
		PixelsPerUnit: 1,
	}

	if err := json.Unmarshal(data, defaultConfig); err != nil {
		return err
	}

	*config = AIModelConfig(*defaultConfig)

	return nil
}

func ParseAIModelConfigs(config string) ([]AIModelConfig, error) {
	var configs []AIModelConfig

	info, err := os.Stat(config)
	if err == nil && !info.IsDir() {
		data, err := os.ReadFile(config)
		if err != nil {
			glog.Error(err)
			return nil, err
		}

		if err := json.Unmarshal(data, &configs); err != nil {
			glog.Error(err)
			return nil, err
		}
	} else {
		if info == nil {
			//config is a string not a file, try to parse it

			if err := json.Unmarshal([]byte(config), &configs); err != nil {
				glog.Error(err)
				return nil, err
			}
		}
	}

	return configs, nil
}

func (n *LivepeerNode) AddAIConfigs(ctx context.Context, configs []AIModelConfig) ([]Capability, map[Capability]*Constraints, error) {
	var aiCaps []Capability
	constraints := make(map[Capability]*Constraints)
	for _, config := range configs {
		modelConstraint := &ModelConstraint{Warm: config.Warm, Capacity: 1}

		// If the config contains a URL we call Warm() anyway because AIWorker will just register
		// the endpoint for an external container
		if config.Warm || config.URL != "" {
			endpoint := worker.RunnerEndpoint{URL: config.URL, Token: config.Token}
			if err := n.AIWorker.Warm(ctx, config.Pipeline, config.ModelID, endpoint, config.OptimizationFlags); err != nil {
				return nil, nil, fmt.Errorf("Error AI worker warming %v container: %v", config.Pipeline, err)
			}
		}

		// Show warning if people set OptimizationFlags but not Warm.
		if len(config.OptimizationFlags) > 0 && !config.Warm {
			glog.Warningf("Model %v has 'optimization_flags' set without 'warm'. Optimization flags are currently only used for warm containers.", config.ModelID)
		}

		pipelineCap := PipelineToCapability(config.Pipeline)

		if pipelineCap > Capability_Unused {
			if config.URL != "" && n.AIWorker.IsRegistered(config.URL, config.Pipeline, config.ModelID) {
				configPrice := big.NewRat(config.PricePerUnit, config.PixelsPerUnit)
				currPrice := n.GetBasePriceForCap("default", pipelineCap, config.ModelID)
				if configPrice.Cmp(currPrice) != 0 {
					n.SetBasePriceForCap("default", pipelineCap, config.ModelID, configPrice)
					glog.V(6).Infof("Capability %s (ID: %v) advertised with model constraint %s price updated to %d per %d unit", config.Pipeline, pipelineCap, config.ModelID, configPrice.Num(), configPrice.Denom())
				}
			} else {
				_, ok := constraints[pipelineCap]
				if !ok {
					aiCaps = append(aiCaps, pipelineCap)
					constraints[pipelineCap] = &Constraints{
						Models: make(map[string]*ModelConstraint),
					}
				}

				if _, ok := constraints[pipelineCap].Models[config.ModelID]; ok {
					if constraints[pipelineCap].Models[config.ModelID].Warm == modelConstraint.Warm {
						constraints[pipelineCap].Models[config.ModelID].Capacity += 1
					} else {
						constraints[pipelineCap].Models[config.ModelID] = modelConstraint
					}
				} else {
					constraints[pipelineCap].Models[config.ModelID] = modelConstraint
				}

				n.SetBasePriceForCap("default", pipelineCap, config.ModelID, big.NewRat(config.PricePerUnit, config.PixelsPerUnit))
			}
		}

		if len(aiCaps) > 0 {
			capability := aiCaps[len(aiCaps)-1]
			price := n.GetBasePriceForCap("default", capability, config.ModelID)
			glog.V(6).Infof("Capability %s (ID: %v) advertised with model constraint %s at price %d per %d unit (%+v)", config.Pipeline, capability, config.ModelID, price.Num(), price.Denom(), modelConstraint)
		}
	}

	return aiCaps, constraints, nil
}

func (n *LivepeerNode) AddAICapabilities(ctx context.Context, aiCaps []Capability, aiConstraints map[Capability]*Constraints) error {

	newCaps := NewCapabilitiesWithConstraints(aiCaps, nil, aiConstraints)

	n.Capabilities.AddCapacity(newCaps)

	var allCaps []Capability
	currentCaps := n.Capabilities.ToNetCapabilities()

	//all capabilities are in capacities (note: capacity is always 1 if the capability exists right now)
	for capability, capacity := range currentCaps.Capacities {
		if capacity > 0 {
			allCaps = append(allCaps, Capability(capability))
		}
	}
	for _, cap := range aiCaps {
		if !slices.Contains(allCaps, cap) {
			allCaps = append(allCaps, cap)
		}
	}

	constraints := currentCaps.Constraints

	for capability, constraint := range aiConstraints {
		_, ok := constraints[uint32(capability)]
		if !ok {
			constraints[uint32(capability)] = &net.Capabilities_Constraints{
				Models: make(map[string]*net.Capabilities_Constraints_ModelConstraint),
			}
		}

		for model_id, modelConstraint := range constraint.Models {
			constraints[uint32(capability)].Models[model_id] = &net.Capabilities_Constraints_ModelConstraint{Warm: modelConstraint.Warm}
		}
	}
	caps := CapabilitiesFromNetCapabilities(currentCaps)

	n.Capabilities = NewCapabilitiesWithConstraints(allCaps, MandatoryOCapabilities(), caps.constraints)

	return nil
}
