package dstack

import (
	"encoding/json"
	"strings"
)

// SSHConnection is the customer-facing SSH endpoint for one job.
// It never carries a direct host address or a private key.
type SSHConnection struct {
	JobID       string   `json:"job_id"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	Username    string   `json:"username"`
	Command     []string `json:"command"`
	CommandLine string   `json:"command_line"`
}

type runSSHResponse struct {
	Jobs []struct {
		ID                string `json:"id"`
		Status            string `json:"status"`
		JobConnectionInfo struct {
			ProxiedSSHCommand  []string `json:"proxied_ssh_command"`
			SSHProxyHostname   string   `json:"sshproxy_hostname"`
			SSHProxyPort       *int     `json:"sshproxy_port"`
			SSHProxyUpstreamID string   `json:"sshproxy_upstream_id"`
		} `json:"job_connection_info"`
	} `json:"jobs"`
}

// ProxiedSSHConnections returns the proxy connection for each running job
// that dstack has prepared an SSH command for. A run without a configured
// proxy returns no connections.
func ProxiedSSHConnections(raw []byte) ([]SSHConnection, error) {
	var response runSSHResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	connections := make([]SSHConnection, 0, len(response.Jobs))
	for _, job := range response.Jobs {
		info := job.JobConnectionInfo
		if len(info.ProxiedSSHCommand) == 0 || info.SSHProxyHostname == "" || info.SSHProxyUpstreamID == "" {
			continue
		}
		port := 22
		if info.SSHProxyPort != nil {
			port = *info.SSHProxyPort
		}
		connections = append(connections, SSHConnection{
			JobID:       job.ID,
			Host:        info.SSHProxyHostname,
			Port:        port,
			Username:    info.SSHProxyUpstreamID,
			Command:     info.ProxiedSSHCommand,
			CommandLine: strings.Join(info.ProxiedSSHCommand, " "),
		})
	}
	return connections, nil
}
