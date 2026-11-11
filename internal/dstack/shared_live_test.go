package dstack

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestLiveSharedProjectProvisioning exercises shared account provisioning against a
// hosted dstack server. It creates and deletes a probe project, so it runs only when
// SKY_PROBE_URL and SKY_PROBE_TOKEN are set.
func TestLiveSharedProjectProvisioning(t *testing.T) {
	baseURL, token := os.Getenv("SKY_PROBE_URL"), os.Getenv("SKY_PROBE_TOKEN")
	if baseURL == "" || token == "" {
		t.Skip("SKY_PROBE_URL and SKY_PROBE_TOKEN are not set")
	}
	client := AdminClient{BaseURL: baseURL, Token: token}
	provisioner := &Provisioner{Admin: client, EncryptionKey: []byte("0123456789abcdef0123456789abcdef"), KeyVersion: "v1"}
	me, err := provisioner.sharedAccount(context.Background())
	if err != nil {
		t.Fatalf("shared account check failed: %v", err)
	}
	name := fmt.Sprintf("1engine-probe-%d", time.Now().UnixNano())
	defer func() {
		if err := client.post(context.Background(), "/api/projects/delete", map[string]any{"projects_names": []string{name}}, nil); err != nil {
			t.Logf("probe project %q cleanup failed: %v", name, err)
		}
	}()
	project, err := provisioner.ensureProject(context.Background(), name)
	if err != nil {
		t.Fatalf("ensure project failed: %v", err)
	}
	if project.ProjectID == "" || project.ProjectName != name {
		t.Fatalf("project = %+v", project)
	}
	if project.Owner.Username != me.Username {
		t.Fatalf("project owner = %q, want %q", project.Owner.Username, me.Username)
	}
	reused, err := provisioner.ensureProject(context.Background(), name)
	if err != nil || reused.ProjectID != project.ProjectID {
		t.Fatalf("reused project = %+v, %v", reused, err)
	}
	var user struct {
		Username string `json:"username"`
	}
	if err := client.post(context.Background(), "/api/users/get_user", map[string]string{"username": name}, &user); err == nil {
		t.Fatalf("shared provisioning created dstack user %q", user.Username)
	}
}
