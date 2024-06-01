package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"strconv"
	"strings"

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
			return nil, err
		}

		if err := json.Unmarshal(data, &configs); err != nil {
			return nil, err
		}

		return configs, nil
	}

	models := strings.Split(config, ",")
	for _, m := range models {
		parts := strings.Split(m, ":")
		if len(parts) < 3 {
			return nil, errors.New("invalid AI model config expected <pipeline>:<model_id>:<warm>")
		}

		pipeline := parts[0]
		modelID := parts[1]
		warm, err := strconv.ParseBool(parts[3])
		if err != nil {
			return nil, err
		}

		configs = append(configs, AIModelConfig{Pipeline: pipeline, ModelID: modelID, Warm: warm})
	}

	return configs, nil
}

func (n *LivepeerNode) AddAIConfigs(ctx context.Context, configs []AIModelConfig) ([]Capability, map[Capability]*Constraints, error) {
	var aiCaps []Capability
	constraints := make(map[Capability]*Constraints)
	for _, config := range configs {
		modelConstraint := &ModelConstraint{Warm: config.Warm}

		// If the config contains a URL we call Warm() anyway because AIWorker will just register
		// the endpoint for an external container
		if config.Warm || config.URL != "" {
			endpoint := worker.RunnerEndpoint{URL: config.URL, Token: config.Token}
			if err := n.AIWorker.Warm(ctx, config.Pipeline, config.ModelID, endpoint, config.OptimizationFlags); err != nil {
				return nil, nil, errors.New(fmt.Sprintf("Error AI worker warming %v container: %v", config.Pipeline, err))
			}
		}

		// Show warning if people set OptimizationFlags but not Warm.
		if len(config.OptimizationFlags) > 0 && !config.Warm {
			glog.Warningf("Model %v has 'optimization_flags' set without 'warm'. Optimization flags are currently only used for warm containers.", config.ModelID)
		}

		switch config.Pipeline {
		case "text-to-image":
			_, ok := constraints[Capability_TextToImage]
			if !ok {
				aiCaps = append(aiCaps, Capability_TextToImage)
				constraints[Capability_TextToImage] = &Constraints{
					Models: make(map[string]*ModelConstraint),
				}
			}

			constraints[Capability_TextToImage].Models[config.ModelID] = modelConstraint

			n.SetBasePriceForCap("default", Capability_TextToImage, config.ModelID, big.NewRat(config.PricePerUnit, config.PixelsPerUnit))
		case "image-to-image":
			_, ok := constraints[Capability_ImageToImage]
			if !ok {
				aiCaps = append(aiCaps, Capability_ImageToImage)
				constraints[Capability_ImageToImage] = &Constraints{
					Models: make(map[string]*ModelConstraint),
				}
			}

			constraints[Capability_ImageToImage].Models[config.ModelID] = modelConstraint

			n.SetBasePriceForCap("default", Capability_ImageToImage, config.ModelID, big.NewRat(config.PricePerUnit, config.PixelsPerUnit))
		case "image-to-video":
			_, ok := constraints[Capability_ImageToVideo]
			if !ok {
				aiCaps = append(aiCaps, Capability_ImageToVideo)
				constraints[Capability_ImageToVideo] = &Constraints{
					Models: make(map[string]*ModelConstraint),
				}
			}

			constraints[Capability_ImageToVideo].Models[config.ModelID] = modelConstraint

			n.SetBasePriceForCap("default", Capability_ImageToVideo, config.ModelID, big.NewRat(config.PricePerUnit, config.PixelsPerUnit))
		}

		if len(aiCaps) > 0 {
			capability := aiCaps[len(aiCaps)-1]
			price := n.GetBasePriceForCap("default", capability, config.ModelID)
			glog.V(6).Infof("Capability %s (ID: %v) advertised with model constraint %s at price %d per %d unit", config.Pipeline, capability, config.ModelID, price.Num(), price.Denom())
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
