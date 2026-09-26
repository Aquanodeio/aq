package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestListDeploymentsDecodesRestoreCompatibilityFromUntransformedRow checks
// that GET /deployments (the list route, which never transforms its rows —
// see the Deployment doc comment) decodes restore_compatibility/
// restore_warnings the same way it decodes restore_status/restore_error.
// This is the surface-the-compatibility-verdict gap: a CUDA-12 environment
// restored onto a CUDA-11 box previously reported nothing anywhere the CLI
// could see, even though the orchestrator persisted the verdict on every row.
func TestListDeploymentsDecodesRestoreCompatibilityFromUntransformedRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[{"id":42,"name":"imported-box","status":"ACTIVE",`+
			`"restore_compatibility":"minor","restore_warnings":["driver CUDA 12.4 on the box vs 12.1 the snapshot expects"]}]}`)
	}))
	defer srv.Close()

	deps, err := NewAuthed(srv.URL, "tok", "t").ListDeployments()
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("got %d deployments, want 1", len(deps))
	}
	if got := deps[0].CompatibilityLabel(); got != "minor" {
		t.Errorf("CompatibilityLabel() = %q, want %q", got, "minor")
	}
	warnings := deps[0].RestoreWarningMessages()
	if len(warnings) != 1 || warnings[0] != "driver CUDA 12.4 on the box vs 12.1 the snapshot expects" {
		t.Errorf("RestoreWarningMessages() = %v, want the one warning", warnings)
	}
}

// TestListDeploymentsToleratesUnexpectedCompatibilityShape checks that a
// surprising restore_compatibility/restore_warnings shape (an object, or a
// bare string for warnings) degrades to an empty verdict rather than failing
// the whole row's decode — the same defensive reasoning ServiceURLs already
// follows, now extended to these two fields.
func TestListDeploymentsToleratesUnexpectedCompatibilityShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[{"id":42,"name":"box","status":"ACTIVE",`+
			`"restore_compatibility":{"level":"minor"},"restore_warnings":"a single warning string"}]}`)
	}))
	defer srv.Close()

	deps, err := NewAuthed(srv.URL, "tok", "t").ListDeployments()
	if err != nil {
		t.Fatalf("ListDeployments must not fail on an unexpected compatibility shape: %v", err)
	}
	if got := deps[0].CompatibilityLabel(); got != "" {
		t.Errorf("CompatibilityLabel() = %q, want \"\" for an object shape", got)
	}
	warnings := deps[0].RestoreWarningMessages()
	if len(warnings) != 1 || warnings[0] != "a single warning string" {
		t.Errorf("RestoreWarningMessages() = %v, want the single string wrapped in a slice", warnings)
	}
}

// TestGetDeploymentReturnsProjectID checks GetDeployment (the raw
// GET /deployments/:id row) decodes project_id — the field `aq pause` needs
// to hit the project-scoped pause route, which the transformed /status and
// list endpoints don't carry.
func TestGetDeploymentReturnsProjectID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/deployments/2884" {
			t.Errorf("path = %q, want /deployments/2884", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":2884,"name":"comfyui","status":"ACTIVE","project_id":"11111111-1111-1111-1111-111111111111"}}`)
	}))
	defer srv.Close()

	dep, err := NewAuthed(srv.URL, "tok", "t").GetDeployment(2884)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if dep.ProjectID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("ProjectID = %q, want the project uuid", dep.ProjectID)
	}
}

// TestPauseDeploymentPostsToProjectScopedPath checks PauseDeployment (`aq
// pause`) hits the existing /deployments/project/:projectId/pause route with
// deploymentId in the body, not a hypothetical deployment-scoped pause
// endpoint.
func TestPauseDeploymentPostsToProjectScopedPath(t *testing.T) {
	var gotPath string
	var gotBody map[string]int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":null}`)
	}))
	defer srv.Close()

	if err := NewAuthed(srv.URL, "tok", "t").PauseDeployment("proj-1", 2884); err != nil {
		t.Fatalf("PauseDeployment: %v", err)
	}
	if gotPath != "/deployments/project/proj-1/pause" {
		t.Errorf("path = %q, want /deployments/project/proj-1/pause", gotPath)
	}
	if gotBody["deploymentId"] != 2884 {
		t.Errorf("body = %+v, want deploymentId 2884", gotBody)
	}
}

// TestGetDeploymentDecodesSetupID and TestDeploymentStatusDecodesSetupID pin
// SetupID against a trimmed copy of a real captured response (prod deployment
// 3807, 2026-09-27, ids kept since they identify our own test-account rows,
// not a customer's): `aq status` needs setup_id to look up the pod's real
// save state (GetSetup), and it is snake_case on BOTH the raw row and the
// nested object /status returns, unlike most of that endpoint's other fields.

func TestGetDeploymentDecodesSetupID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":3807,"name":"MI300X box 11","status":"CLOSED",`+
			`"setup_id":"b8591f0f-038e-4d48-b9a2-26a3b7caeadd"}}`)
	}))
	defer srv.Close()

	dep, err := NewAuthed(srv.URL, "tok", "t").GetDeployment(3807)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if dep.SetupID != "b8591f0f-038e-4d48-b9a2-26a3b7caeadd" {
		t.Errorf("SetupID = %q, want the pod uuid", dep.SetupID)
	}
}

func TestDeploymentStatusDecodesSetupID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"deploymentId":3807,"status":"CLOSED","deployment":`+
			`{"id":3807,"status":"CLOSED","setup_id":"b8591f0f-038e-4d48-b9a2-26a3b7caeadd","deploymentId":3807}}}`)
	}))
	defer srv.Close()

	res, err := NewAuthed(srv.URL, "tok", "t").DeploymentStatus(3807)
	if err != nil {
		t.Fatalf("DeploymentStatus: %v", err)
	}
	if res.Deployment.SetupID != "b8591f0f-038e-4d48-b9a2-26a3b7caeadd" {
		t.Errorf("Deployment.SetupID = %q, want the pod uuid", res.Deployment.SetupID)
	}
}
