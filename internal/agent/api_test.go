package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/objectstore"
)

func TestLocalAPIWorkloadOwnership(t *testing.T) {
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + configuration.StateDir + "/agent.sock"
	configuration.InsecureDevelopment = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := Open(configuration, logger)
	if err != nil {
		t.Fatal(err)
	}
	workloadRoot := t.TempDir()
	spec := model.WorkloadSpec{
		Name: "owned", Command: []string{"/bin/true"}, RootPath: workloadRoot,
		WorkingDir: workloadRoot, UID: os.Geteuid(), GID: os.Getegid(),
	}
	body, _ := json.Marshal(spec)
	request := httptest.NewRequest(http.MethodPost, "/v1/workloads", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), credentialsContextKey{}, peerCredentials{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}))
	response := httptest.NewRecorder()
	service.localHandler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", response.Code, response.Body.String())
	}
	var created model.Workload
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRequest(http.MethodGet, "/v1/workloads/"+created.Spec.ID, nil)
	unauthorized = unauthorized.WithContext(context.WithValue(unauthorized.Context(), credentialsContextKey{}, peerCredentials{UID: uint32(os.Geteuid() + 1), GID: uint32(os.Getegid() + 1)}))
	unauthorizedResponse := httptest.NewRecorder()
	service.localHandler().ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-user access status %d: %s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}
}

func TestHealthRequiresLocalIdentity(t *testing.T) {
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + configuration.StateDir + "/agent.sock"
	configuration.InsecureDevelopment = false
	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	response := httptest.NewRecorder()
	service.localHandler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous health status %d", response.Code)
	}
}

func TestLocalAPIRetriesCheckpointMirror(t *testing.T) {
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + filepath.Join(configuration.StateDir, "agent.sock")
	configuration.InsecureDevelopment = true
	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: "checkpoint-api", Kind: model.CheckpointFull, CreatedAt: time.Now().UTC(),
		Workload: model.WorkloadSpec{
			ID: "workload-api", Name: "api-mirror", RootPath: t.TempDir(),
			UID: os.Geteuid(), GID: os.Getegid(),
		},
		SourceIdentity: service.identity.Machine,
	}
	if err := checkpoint.SignManifest(&manifest, service.identity); err != nil {
		t.Fatal(err)
	}
	if err := service.repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	remote, err := objectstore.OpenLocal(filepath.Join(configuration.StateDir, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	service.checkpoints.SetMirror(checkpoint.NewMirror(remote, service.chunks, service.repository))

	request := httptest.NewRequest(http.MethodPost, "/v1/checkpoints/"+manifest.ID+"/mirror", nil)
	request = request.WithContext(context.WithValue(request.Context(), credentialsContextKey{}, peerCredentials{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}))
	response := httptest.NewRecorder()
	service.localHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("mirror status %d: %s", response.Code, response.Body.String())
	}
	var result checkpoint.MirrorResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.CheckpointID != manifest.ID || result.Objects != 1 {
		t.Fatalf("unexpected mirror result: %+v", result)
	}
	if _, err := remote.Head(context.Background(), "checkpoints/"+manifest.ID+"/manifest.enc.json"); err != nil {
		t.Fatal(err)
	}
}
