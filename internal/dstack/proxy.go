package dstack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"billing-backend/internal/domain"
	"billing-backend/internal/ledger"
)

type OperationKind string

const (
	OperationRead     OperationKind = "read"
	OperationStart    OperationKind = "compute-start"
	OperationStop     OperationKind = "compute-stop"
	OperationMutation OperationKind = "tenant-mutation"
)

type RoutePolicy struct {
	Method         string
	Pattern        *regexp.Regexp
	Kind           OperationKind
	ProjectCapture int
	UserCapture    int
}
type Operation struct {
	Method         string
	Path           string
	Query          url.Values
	Body           []byte
	Headers        http.Header
	IdempotencyKey string
}
type SafeResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}
type CredentialSource interface {
	CustomerCredential(context.Context, domain.TenantID, []byte) (string, error)
}
type Proxy struct {
	BaseURL       string
	HTTP          *http.Client
	Credentials   CredentialSource
	Backends      ProviderBackends
	EncryptionKey []byte
	Ledger        ledger.ReservationService
	Policies      []RoutePolicy
	PriceKey      string
}

type NoRedirectClient struct{ Client *http.Client }

func (c NoRedirectClient) Do(req *http.Request) (*http.Response, error) { return c.Client.Do(req) }

func defaultPolicies() []RoutePolicy {
	pattern := func(method, path string, kind OperationKind) RoutePolicy {
		return RoutePolicy{Method: method, Pattern: regexp.MustCompile(path), Kind: kind}
	}
	return []RoutePolicy{
		pattern("POST", `^/api/runs/list$`, OperationRead),
		pattern("POST", `^/api/instances/list$`, OperationRead),
		pattern("POST", `^/api/events/list$`, OperationRead),
		pattern("POST", `^/api/projects/[^/]+/get$`, OperationRead),
		pattern("POST", `^/api/project/[^/]+/(runs/(get|get_plan)|fleets/(list|get|get_plan)|instances/(get|get_instance_health_checks)|repos/(list|get)|gpus/list|logs/poll|secrets/(list|get)|gateways/(list|get|get_plan)|volumes/(list|get)|templates/list|exports/list)$`, OperationRead),
		pattern("POST", `^/api/project/[^/]+/runs/apply$`, OperationStart),
		pattern("POST", `^/api/project/[^/]+/fleets/apply$`, OperationStart),
		pattern("POST", `^/api/project/[^/]+/gateways/(apply|create)$`, OperationStart),
		pattern("POST", `^/api/project/[^/]+/(runs/(stop|delete)|fleets/(delete|delete_instances)|volumes/delete|gateways/delete|secrets/delete|repos/delete|exports/delete|imports/delete)$`, OperationStop),
		pattern("POST", `^/api/project/[^/]+/(presets/push|secrets/create_or_update|repos/(init|upload_code)|exports/(create|update)|gateways/(set_default|set_wildcard_domain))$`, OperationMutation),
		pattern("POST", `^/api/project/[^/]+/volumes/create$`, OperationMutation),
		pattern("POST", `^/api/project/[^/]+/presets/(get|get_files)$`, OperationRead),
		pattern("POST", `^/api/users/public_keys/list$`, OperationRead),
		pattern("POST", `^/api/users/public_keys/(add|delete)$`, OperationMutation),
		pattern("GET", `^/api/project/[^/]+/metrics/job/[^/]+$`, OperationRead),
		pattern("GET", `^/proxy/models/[^/]+/models$`, OperationRead),
		pattern("POST", `^/proxy/models/[^/]+/chat/completions$`, OperationStart),
		pattern("GET", `^/proxy/services/[^/]+/[^/]+(/[^/]+)?$`, OperationRead),
		pattern("POST", `^/proxy/services/[^/]+/[^/]+(/[^/]+)?$`, OperationStart),
		pattern("PUT", `^/proxy/services/[^/]+/[^/]+(/[^/]+)?$`, OperationStart),
		pattern("PATCH", `^/proxy/services/[^/]+/[^/]+(/[^/]+)?$`, OperationStart),
		pattern("DELETE", `^/proxy/services/[^/]+/[^/]+(/[^/]+)?$`, OperationStop),
	}
}

func NewProxy(baseURL string, credentials CredentialSource, key []byte, ledgerService *ledger.Service) *Proxy {
	return &Proxy{BaseURL: baseURL, Credentials: credentials, EncryptionKey: key, Ledger: ledgerService, Policies: defaultPolicies(), HTTP: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, PriceKey: "default"}
}

type backendPlanKind uint8

const (
	fleetPlan backendPlanKind = iota + 1
	runPlan
)

type backendPlanRoute struct {
	kind          backendPlanKind
	pathSuffix    string
	accessors     []string
	configuration bool
}

var backendPlanRoutes = []backendPlanRoute{
	{kind: fleetPlan, pathSuffix: "/fleets/get_plan", accessors: []string{"spec"}, configuration: true},
	{kind: fleetPlan, pathSuffix: "/fleets/apply", accessors: []string{"plan", "spec"}, configuration: true},
	{kind: runPlan, pathSuffix: "/runs/get_plan", accessors: []string{"run_spec"}},
	{kind: runPlan, pathSuffix: "/runs/apply", accessors: []string{"plan", "run_spec"}},
}

func backendPlanForPath(path string) (backendPlanRoute, bool) {
	for _, route := range backendPlanRoutes {
		if strings.HasSuffix(path, route.pathSuffix) {
			return route, true
		}
	}
	return backendPlanRoute{}, false
}

func (p *Proxy) bindProviderBackends(raw []byte, path string) ([]byte, error) {
	route, ok := backendPlanForPath(path)
	if !ok {
		return raw, nil
	}
	if len(p.Backends.Types()) == 0 {
		return nil, errors.New("no compute backends are configured")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("dstack request body must be valid JSON")
	}
	spec := objectAt(body, route.accessors...)
	if spec == nil {
		return nil, errors.New("dstack fleet or run plan is missing its specification")
	}
	configuration, ok := spec["configuration"].(map[string]any)
	if !ok {
		return nil, errors.New("dstack plan is missing its configuration")
	}
	if route.configuration {
		if sshConfig, exists := configuration["ssh_config"]; exists && sshConfig != nil {
			return nil, errors.New("customer SSH fleets are not supported")
		}
	}
	profile, _ := spec["profile"].(map[string]any)
	if err := bindConfiguredBackends(configuration, profile, p.Backends); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("dstack request body could not be encoded")
	}
	return encoded, nil
}

func objectAt(value map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		next, ok := value[key].(map[string]any)
		if !ok {
			return nil
		}
		value = next
	}
	return value
}

func configuredBackendSelection(spec map[string]any, allowed ProviderBackends) ([]string, bool, error) {
	raw, exists := spec["backends"]
	if !exists {
		return nil, false, nil
	}
	requested, ok := raw.([]any)
	if !ok || len(requested) == 0 {
		return nil, true, errors.New("dstack backends must be a non-empty list of backend types")
	}
	selected := make([]string, 0, len(requested))
	for _, value := range requested {
		backendType, ok := value.(string)
		if !ok || !allowed.Allows(backendType) {
			return nil, true, errors.New("fleet or run selects a backend that is not enabled")
		}
		if slices.Contains(selected, backendType) {
			return nil, true, errors.New("dstack backend selection contains duplicates")
		}
		selected = append(selected, backendType)
	}
	return selected, true, nil
}

func bindConfiguredBackends(configuration, profile map[string]any, allowed ProviderBackends) error {
	configurationTypes, configurationSet, err := configuredBackendSelection(configuration, allowed)
	if err != nil {
		return err
	}
	profileTypes, profileSet := []string(nil), false
	if profile != nil {
		profileTypes, profileSet, err = configuredBackendSelection(profile, allowed)
		if err != nil {
			return err
		}
	}
	if configurationSet && profileSet && !slices.Equal(configurationTypes, profileTypes) {
		setConfiguredBackendSelection(profile, configurationTypes)
		return nil
	}
	if configurationSet || profileSet {
		return nil
	}
	configured := backendTypes(allowed.Types())
	setConfiguredBackendSelection(configuration, configured)
	if profile != nil {
		setConfiguredBackendSelection(profile, configured)
	}
	return nil
}

func backendTypes(types []BackendType) []string {
	result := make([]string, 0, len(types))
	for _, backendType := range types {
		result = append(result, string(backendType))
	}
	return result
}

func setConfiguredBackendSelection(spec map[string]any, types []string) {
	backends := make([]any, 0, len(types))
	for _, backendType := range types {
		backends = append(backends, backendType)
	}
	spec["backends"] = backends
}

func (p *Proxy) classify(method, path, project string) (OperationKind, error) {
	for _, policy := range p.Policies {
		if method == policy.Method && policy.Pattern.MatchString(path) {
			if projectPath(path) {
				parts := strings.Split(path, "/")
				if len(parts) <= 3 || parts[3] == "" {
					return "", errors.New("invalid dstack project path")
				}
				if parts[3] != project || (parts[2] == "projects" && (len(parts) != 5 || parts[4] != "get")) {
					return "", errors.New("customer request cannot select another dstack project")
				}
			}
			return policy.Kind, nil
		}
	}
	return "", errors.New("dstack route is not allowed")
}

func (p *Proxy) Execute(ctx context.Context, tenant domain.TenantID, binding domain.DStackBinding, op Operation) (SafeResponse, error) {
	if !strings.HasPrefix(op.Path, "/") || strings.HasPrefix(op.Path, "//") || strings.Contains(op.Path, "?") || strings.Contains(op.Path, "#") || strings.Contains(op.Path, "\\") || strings.Contains(strings.ToLower(op.Path), "%2f") || strings.Contains(strings.ToLower(op.Path), "%5c") {
		return SafeResponse{}, errors.New("dstack route path is invalid")
	}
	for _, segment := range strings.Split(op.Path, "/") {
		if segment == "." || segment == ".." {
			return SafeResponse{}, errors.New("dstack route path is invalid")
		}
	}
	kind, err := p.classify(op.Method, op.Path, string(binding.Project))
	if err != nil {
		return SafeResponse{}, err
	}
	if binding.Provision != domain.ProvisionReady {
		return SafeResponse{}, domain.ErrTenantUnavailable
	}
	path := op.Path
	if projectPath(path) {
		parts := strings.Split(path, "/")
		if len(parts) < 4 || parts[3] == "" {
			return SafeResponse{}, errors.New("invalid dstack project path")
		}
		if parts[3] != string(binding.Project) {
			return SafeResponse{}, errors.New("customer request cannot select another dstack project")
		}
		parts[3] = url.PathEscape(parts[3])
		path = strings.Join(parts, "/")
	}
	if strings.HasPrefix(path, "/proxy/") {
		parts := strings.Split(path, "/")
		if len(parts) < 5 || parts[3] != string(binding.Project) {
			return SafeResponse{}, errors.New("customer request cannot select another dstack project")
		}
		parts[3] = url.PathEscape(parts[3])
		path = strings.Join(parts, "/")
	}
	if len(op.Query) > 0 {
		return SafeResponse{}, errors.New("dstack customer operations do not accept URL query parameters")
	}
	if kind == OperationStart && strings.TrimSpace(op.IdempotencyKey) == "" {
		return SafeResponse{}, errors.New("compute start requires an Idempotency-Key")
	}
	if _, isPlan := backendPlanForPath(op.Path); isPlan {
		if len(op.Body) == 0 || !strings.Contains(strings.ToLower(op.Headers.Get("Content-Type")), "json") {
			return SafeResponse{}, errors.New("dstack fleet and run plans require a JSON body")
		}
		if !json.Valid(op.Body) {
			return SafeResponse{}, errors.New("dstack request body must be valid JSON")
		}
		op.Body, err = p.bindProviderBackends(op.Body, op.Path)
		if err != nil {
			return SafeResponse{}, err
		}
	}
	if p.Credentials == nil {
		return SafeResponse{}, domain.ErrTenantUnavailable
	}
	base, parseErr := url.Parse(p.BaseURL)
	if parseErr != nil || base.Scheme != "https" || base.Host == "" || base.User != nil {
		return SafeResponse{}, errors.New("dstack endpoint must be an absolute HTTPS URL")
	}
	credential, err := p.Credentials.CustomerCredential(ctx, tenant, p.EncryptionKey)
	if err != nil || credential == "" {
		return SafeResponse{}, domain.ErrTenantUnavailable
	}
	if len(op.Body) > 0 {
		if strings.Contains(strings.ToLower(op.Headers.Get("Content-Type")), "json") {
			op.Body, err = bindProject(op.Body, op.Path, string(binding.Project), binding.ProjectID, binding.Username)
		} else {
			return SafeResponse{}, errors.New("dstack proxy accepts JSON request bodies only")
		}
		if err != nil {
			return SafeResponse{}, err
		}
	}
	if len(op.Body) == 0 && op.Method == http.MethodPost {
		return SafeResponse{}, errors.New("dstack customer operation requires a JSON request body")
	}
	if len(op.Body) > 0 && !strings.Contains(strings.ToLower(op.Headers.Get("Content-Type")), "json") {
		return SafeResponse{}, errors.New("dstack proxy accepts JSON request bodies only")
	}
	if len(op.Query) > 0 {
		return SafeResponse{}, errors.New("dstack customer operations do not accept URL query parameters")
	}
	reservationKey := ""
	if kind == OperationStart {
		reservationKey = strings.TrimSpace(op.IdempotencyKey)
		if reservationKey == "" || len(reservationKey) > 200 {
			return SafeResponse{}, errors.New("compute start requires an Idempotency-Key of 1 to 200 characters")
		}
		if p.Ledger == nil {
			return SafeResponse{}, domain.ErrPriceUnverified
		}
		if len(op.Body) == 0 || !json.Valid(op.Body) {
			return SafeResponse{}, errors.New("dstack request body must be valid JSON")
		}
		identity := strings.Join([]string{op.Method, op.Path, op.Query.Encode(), string(op.Body)}, "\n")
		hash := sha256.Sum256([]byte(identity))
		reservation, reserveErr := p.Ledger.Reserve(ctx, tenant, p.PriceKey, reservationKey, hash[:], time.Hour)
		if reserveErr != nil {
			return SafeResponse{}, reserveErr
		}
		if reservation.Replayed {
			return SafeResponse{Status: reservation.ResponseStatus, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: reservation.ResponseBody}, nil
		}
	}
	endpoint, err := url.Parse(strings.TrimRight(p.BaseURL, "/") + path)
	if err != nil {
		return SafeResponse{}, err
	}
	if endpoint.Scheme != "https" || endpoint.Host == "" {
		return SafeResponse{}, errors.New("dstack endpoint must use HTTPS")
	}
	endpoint.RawQuery = op.Query.Encode()
	request, err := http.NewRequestWithContext(ctx, op.Method, endpoint.String(), bytes.NewReader(op.Body))
	if err != nil {
		return SafeResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/json")
	if ct := op.Headers.Get("Content-Type"); ct != "" {
		request.Header.Set("Content-Type", ct)
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := client.Do(request)
	if err != nil {
		if reservationKey != "" {
			_ = p.Ledger.MarkReservationUncertain(ctx, tenant, reservationKey)
		}
		return SafeResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return SafeResponse{}, err
	}
	if strings.Contains(response.Header.Get("Content-Type"), "json") {
		body, err = redactJSON(body)
		if err != nil {
			return SafeResponse{}, fmt.Errorf("dstack returned invalid JSON")
		}
	}
	if reservationKey != "" {
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if body == nil {
				body = []byte{}
			}
			if err := p.Ledger.CompleteReservation(ctx, tenant, reservationKey, response.StatusCode, body); err != nil {
				return SafeResponse{}, err
			}
		} else if response.StatusCode < 500 {
			if err := p.Ledger.ReleaseReservation(ctx, tenant, reservationKey); err != nil {
				return SafeResponse{}, err
			}
		}
	}
	if response.StatusCode == http.StatusNoContent {
		body = nil
	}
	headers := make(http.Header)
	for _, name := range []string{"Content-Type", "ETag", "Last-Modified", "Cache-Control"} {
		if value := response.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	return SafeResponse{Status: response.StatusCode, Headers: headers, Body: body}, nil
}

func projectPath(path string) bool {
	return strings.HasPrefix(path, "/api/project/") || strings.HasPrefix(path, "/api/projects/") || strings.HasPrefix(path, "/proxy/")
}

func bindProject(raw []byte, path, project, projectID, username string) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("dstack request body must be valid JSON")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("dstack request body must be a JSON object")
	}
	set := func(key, expected string) error {
		if supplied, exists := object[key]; exists {
			name, ok := supplied.(string)
			if !ok || (name != "" && name != expected) {
				return errors.New("customer request cannot select another dstack tenant")
			}
		}
		object[key] = expected
		return nil
	}
	switch {
	case strings.HasPrefix(path, "/api/projects/"):
		if len(object) != 0 {
			return nil, errors.New("project detail request must be empty")
		}
	case path == "/api/instances/list":
		if rawProjects, exists := object["project_names"]; exists {
			projects, ok := rawProjects.([]any)
			if !ok {
				return nil, errors.New("customer request cannot select another dstack project")
			}
			for _, value := range projects {
				if name, ok := value.(string); !ok || name != project {
					return nil, errors.New("customer request cannot select another dstack project")
				}
			}
		}
		object["project_names"] = []string{project}
		if _, exists := object["only_active"]; !exists {
			object["only_active"] = true
		}
	case path == "/api/events/list":
		if projectID == "" {
			return nil, errors.New("dstack project ID is required to scope events")
		}
		if rawProjects, exists := object["within_projects"]; exists {
			projects, ok := rawProjects.([]any)
			if !ok {
				return nil, errors.New("customer request cannot select another dstack project")
			}
			for _, value := range projects {
				if id, ok := value.(string); !ok || id != projectID {
					return nil, errors.New("customer request cannot select another dstack project")
				}
			}
		}
		object["within_projects"] = []string{projectID}
	case path == "/api/users/public_keys/list" || path == "/api/users/public_keys/add" || path == "/api/users/public_keys/delete":
		if err := set("username", username); err != nil {
			return nil, err
		}
	case path == "/api/runs/list":
		if err := set("project_name", project); err != nil {
			return nil, err
		}
		if err := set("username", username); err != nil {
			return nil, err
		}
		if _, exists := object["only_active"]; !exists {
			object["only_active"] = true
		}
	case strings.HasSuffix(path, "/runs/apply"):
		if _, exists := object["plan"]; !exists {
			return nil, errors.New("dstack run apply requires a plan")
		}
		if err := bindNestedTenantReferences(object["plan"], project, username); err != nil {
			return nil, err
		}
		force, ok := object["force"].(bool)
		if !ok || force {
			return nil, errors.New("dstack run apply must provide force=false")
		}
	default:
		for _, key := range []string{"project_name", "project"} {
			if _, exists := object[key]; exists {
				if err := set(key, project); err != nil {
					return nil, err
				}
			}
		}
		if _, exists := object["username"]; exists {
			if err := set("username", username); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(object)
}

func bindNestedTenantReferences(node any, project, username string) error {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			lower := strings.ToLower(key)
			if lower == "project" || lower == "project_name" {
				name, ok := child.(string)
				if !ok || (name != "" && name != project) {
					return errors.New("customer request cannot select another dstack project")
				}
				value[key] = project
			}
			if lower == "user" || lower == "username" {
				name, ok := child.(string)
				if !ok || (name != "" && name != username) {
					return errors.New("customer request cannot select another dstack user")
				}
				value[key] = username
			}
			if err := bindNestedTenantReferences(child, project, username); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := bindNestedTenantReferences(child, project, username); err != nil {
				return err
			}
		}
	}
	return nil
}

func redactJSON(raw []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	var clean func(any) any
	clean = func(node any) any {
		switch v := node.(type) {
		case map[string]any:
			out := make(map[string]any, len(v))
			for k, val := range v {
				lower := strings.ToLower(k)
				if strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "credential") || lower == "authorization" {
					continue
				}
				out[k] = clean(val)
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i, val := range v {
				out[i] = clean(val)
			}
			return out
		default:
			return node
		}
	}
	return json.Marshal(clean(value))
}
