package dstack

import (
	"errors"
	"net/url"
	"strings"
)

// ServerMode selects how the backend reaches a dstack server. A server that grants
// global admin uses admin_refresh; a server that gives one ordinary account uses
// shared_project.
type ServerMode string

const (
	ModeAdminRefresh  ServerMode = "admin_refresh"
	ModeSharedProject ServerMode = "shared_project"
)

// Server is one dstack server that can host tenant projects and supply fleet instances.
type Server struct {
	Name     string
	URL      string
	Token    string
	Mode     ServerMode
	Backends ProviderBackends
}

// Servers is the ordered set of dstack servers the backend can place workloads on.
type Servers struct {
	byName map[string]Server
	order  []string
}

func NewServers(defs ...Server) (Servers, error) {
	servers := Servers{byName: make(map[string]Server, len(defs)), order: make([]string, 0, len(defs))}
	for _, def := range defs {
		if err := validateServer(def); err != nil {
			return Servers{}, err
		}
		if _, exists := servers.byName[def.Name]; exists {
			return Servers{}, errors.New("dstack server name is configured more than once")
		}
		servers.byName[def.Name] = def
		servers.order = append(servers.order, def.Name)
	}
	return servers, nil
}

func validateServer(def Server) error {
	if strings.TrimSpace(def.Name) == "" {
		return errors.New("dstack server name is required")
	}
	base, err := url.Parse(def.URL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil {
		return errors.New("dstack server URL must be an absolute HTTPS URL")
	}
	if strings.TrimSpace(def.Token) == "" {
		return errors.New("dstack server token is required")
	}
	switch def.Mode {
	case ModeAdminRefresh:
	case ModeSharedProject:
		if len(def.Backends.Types()) != 0 {
			return errors.New("a shared-project dstack server must not configure provider backends")
		}
	default:
		return errors.New("dstack server mode must be admin_refresh or shared_project")
	}
	return nil
}

func (s Servers) Get(name string) (Server, bool) {
	server, ok := s.byName[name]
	return server, ok
}

func (s Servers) Names() []string {
	return append([]string(nil), s.order...)
}

// Default is the server that new tenants use when no rule chooses another one.
func (s Servers) Default() (Server, bool) {
	if len(s.order) == 0 {
		return Server{}, false
	}
	return s.byName[s.order[0]], true
}
