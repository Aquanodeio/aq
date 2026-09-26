package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestKeepSetupEnvironmentPostsName checks POST /setups/:id/environment/keep
// sends {name} and decodes {environmentId} — Keep only names the pod's
// current environment, it mints no version.
func TestKeepSetupEnvironmentPostsName(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"environmentId":"env-1"}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").KeepSetupEnvironment("pod-1", "pytorch-dev")
	if err != nil {
		t.Fatalf("KeepSetupEnvironment: %v", err)
	}
	if gotPath != "/setups/pod-1/environment/keep" {
		t.Errorf("path = %q, want /setups/pod-1/environment/keep", gotPath)
	}
	if gotBody["name"] != "pytorch-dev" {
		t.Errorf("body = %#v, want name=pytorch-dev", gotBody)
	}
	if got.EnvironmentID != "env-1" {
		t.Errorf("EnvironmentID = %q, want env-1", got.EnvironmentID)
	}
}

// TestShareSetupEnvironmentOmitsVersionIDWhenAbsent checks POST
// /setups/:id/environment/share omits versionId entirely when none is
// given, so the server reads "share the latest" rather than an empty string
// it might reject as an invalid id.
func TestShareSetupEnvironmentOmitsVersionIDWhenAbsent(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"shareId":"share-1","shareUrl":"https://console.aquanode.io/launch/tok","state":"preparing"}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").ShareSetupEnvironment("pod-1", "")
	if err != nil {
		t.Fatalf("ShareSetupEnvironment: %v", err)
	}
	if _, present := gotBody["versionId"]; present {
		t.Errorf("versionId must be absent when unset, got: %#v", gotBody)
	}
	if got.State != "preparing" || got.ShareID != "share-1" {
		t.Errorf("result = %+v", got)
	}
}

// TestShareEnvironmentByIDRequiresVersionOnTheWire checks POST
// /environments/:id/share always sends versionId (never omitempty) — unlike
// the pod-scoped route, a named environment has no single "current pod" to
// default the latest version from, so the caller must always supply one.
func TestShareEnvironmentByIDRequiresVersionOnTheWire(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"shareId":"share-2","shareUrl":"https://console.aquanode.io/launch/tok2","state":"ready"}}`)
	}))
	defer srv.Close()

	if _, err := NewAuthed(srv.URL, "tok", "t").ShareEnvironmentByID("env-1", "v-3"); err != nil {
		t.Fatalf("ShareEnvironmentByID: %v", err)
	}
	if gotPath != "/environments/env-1/share" {
		t.Errorf("path = %q, want /environments/env-1/share", gotPath)
	}
	if gotBody["versionId"] != "v-3" {
		t.Errorf("body = %#v, want versionId=v-3", gotBody)
	}
}

// TestGetShareStatusDecodesThreeStates checks GET /shares/:shareId decodes
// all three states, including a non-nil Error only on "failed" — a share
// link must never be handed out while State is anything but "ready".
func TestGetShareStatusDecodesThreeStates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"state":"failed","error":"publish job timed out"}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").GetShareStatus("share-1")
	if err != nil {
		t.Fatalf("GetShareStatus: %v", err)
	}
	if got.State != "failed" || got.Error == nil || *got.Error != "publish job timed out" {
		t.Errorf("result = %+v", got)
	}
}

// TestListEnvironmentsDecodesThreeGroups checks GET /environments decodes
// builtin/yours/shared as three DISTINCT groups — the New pod picker's whole
// vocabulary, and a caller that merges them can no longer tell "ours" from
// "someone else's".
func TestListEnvironmentsDecodesThreeGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/environments" {
			t.Errorf("path = %q, want /environments", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{
			"builtin":[{"id":"b1","name":"comfyui"}],
			"yours":[{"id":"y1","name":"pytorch-dev"}],
			"shared":[{"id":"s1","name":"team-llm"}]
		}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").ListEnvironments()
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(got.Builtin) != 1 || got.Builtin[0].Name != "comfyui" {
		t.Errorf("Builtin = %+v", got.Builtin)
	}
	if len(got.Yours) != 1 || got.Yours[0].Name != "pytorch-dev" {
		t.Errorf("Yours = %+v", got.Yours)
	}
	if len(got.Shared) != 1 || got.Shared[0].Name != "team-llm" {
		t.Errorf("Shared = %+v", got.Shared)
	}
}

// TestDeleteEnvironmentSendsDelete checks DELETE /environments/:id.
func TestDeleteEnvironmentSendsDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":null}`)
	}))
	defer srv.Close()

	if err := NewAuthed(srv.URL, "tok", "t").DeleteEnvironment("env-1"); err != nil {
		t.Fatalf("DeleteEnvironment: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/environments/env-1" {
		t.Errorf("%s %s, want DELETE /environments/env-1", gotMethod, gotPath)
	}
}
