package dstack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type BackendType string

const (
	BackendAWS    BackendType = "aws"
	BackendGCP    BackendType = "gcp"
	BackendNebius BackendType = "nebius"
	BackendRunPod BackendType = "runpod"
	BackendVastAI BackendType = "vastai"
)

var supportedBackendTypes = map[BackendType]struct{}{
	BackendAWS: {}, BackendGCP: {}, BackendNebius: {}, BackendRunPod: {}, BackendVastAI: {},
}

type ProviderBackend struct {
	Type BackendType
	YAML string
}

type ProviderBackends struct {
	byType map[BackendType]ProviderBackend
}

func LoadProviderBackends(path string) (ProviderBackends, error) {
	if strings.TrimSpace(path) == "" {
		return ProviderBackends{}, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ProviderBackends{}, fmt.Errorf("read provider backend config: %w", err)
	}
	return ParseProviderBackends(body)
}

func ParseProviderBackends(body []byte) (ProviderBackends, error) {
	var file struct {
		Backends []yaml.Node `yaml:"backends"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			return ProviderBackends{}, nil
		}
		return ProviderBackends{}, errors.New("provider backend config is invalid YAML")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ProviderBackends{}, errors.New("provider backend config must contain one YAML document")
	}
	if len(file.Backends) > len(supportedBackendTypes) {
		return ProviderBackends{}, errors.New("provider backend config contains too many backends")
	}
	backends := ProviderBackends{byType: make(map[BackendType]ProviderBackend, len(file.Backends))}
	for _, node := range file.Backends {
		if node.Kind != yaml.MappingNode {
			return ProviderBackends{}, errors.New("each provider backend must be a YAML mapping")
		}
		backendType, err := backendTypeFromNode(&node)
		if err != nil {
			return ProviderBackends{}, err
		}
		if _, supported := supportedBackendTypes[backendType]; !supported {
			return ProviderBackends{}, fmt.Errorf("unsupported provider backend type %q", backendType)
		}
		if _, exists := backends.byType[backendType]; exists {
			return ProviderBackends{}, fmt.Errorf("provider backend type %q is configured more than once", backendType)
		}
		var config map[string]any
		if err := node.Decode(&config); err != nil {
			return ProviderBackends{}, errors.New("provider backend config is invalid YAML")
		}
		encoded, err := yaml.Marshal(config)
		if err != nil {
			return ProviderBackends{}, errors.New("provider backend config is invalid YAML")
		}
		backends.byType[backendType] = ProviderBackend{Type: backendType, YAML: string(encoded)}
	}
	return backends, nil
}

func backendTypeFromNode(node *yaml.Node) (BackendType, error) {
	if node.Kind != yaml.MappingNode {
		return "", errors.New("each provider backend must be a YAML mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "type" {
			if node.Content[i+1].Kind != yaml.ScalarNode || node.Content[i+1].Tag != "!!str" {
				return "", errors.New("provider backend type must be a string")
			}
			return BackendType(node.Content[i+1].Value), nil
		}
	}
	return "", errors.New("provider backend type is required")
}

func (b ProviderBackends) Types() []BackendType {
	types := make([]BackendType, 0, len(b.byType))
	for backendType := range b.byType {
		types = append(types, backendType)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	return types
}

func (b ProviderBackends) Get(backendType BackendType) (ProviderBackend, bool) {
	backend, ok := b.byType[backendType]
	return backend, ok
}

func (b ProviderBackends) Allows(backendType string) bool {
	_, ok := b.byType[BackendType(backendType)]
	return ok
}
