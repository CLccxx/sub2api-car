//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type carReleaseStub struct {
	updateServiceGitHubClientStub
	repository string
}

func (s *carReleaseStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.repository = repo
	return s.release, nil
}

func completeCarRelease(version string) *GitHubRelease {
	name := "sub2api-car-" + version + "-linux-amd64.tar.gz"
	r := &GitHubRelease{TagName: "v" + version, HTMLURL: "https://github.com/" + carGitHubRepo + "/releases/tag/v" + version}
	for _, asset := range []string{name, name + ".sha256"} {
		r.Assets = append(r.Assets, GitHubAsset{Name: asset, BrowserDownloadURL: "https://github.com/" + carGitHubRepo + "/releases/download/v" + version + "/" + asset, Size: 100})
	}
	return r
}

func TestCarUpdateUsesForkAndRejectsUpstreamCache(t *testing.T) {
	cache := &updateServiceCacheStub{data: fmt.Sprintf(`{"latest":"99.0.0","timestamp":%d,"repository":"Wei-Shaw/sub2api"}`, time.Now().Unix())}
	client := &carReleaseStub{updateServiceGitHubClientStub: updateServiceGitHubClientStub{release: completeCarRelease("0.2.17-car.7")}}
	svc := NewUpdateService(cache, client, "0.2.17-car.6", "release")
	info, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, carGitHubRepo, client.repository)
	require.Equal(t, carGitHubRepo, info.Repository)
	require.Equal(t, "managed", info.UpdateMode)
	require.Equal(t, "0.2.17-car.7", info.LatestVersion)
	require.True(t, info.HasUpdate)
	require.False(t, info.Cached)
	info, err = svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.True(t, info.Cached)
}

func TestCarUpdateRejectsIncompleteOrForeignRelease(t *testing.T) {
	for _, mutate := range []func(*GitHubRelease){
		func(r *GitHubRelease) { r.TagName = "v0.2.18" },
		func(r *GitHubRelease) { r.Prerelease = true },
		func(r *GitHubRelease) { r.Draft = true },
		func(r *GitHubRelease) { r.Assets = r.Assets[:1] },
		func(r *GitHubRelease) { r.Assets[0].BrowserDownloadURL = "https://github.com/other/repo/asset" },
		func(r *GitHubRelease) { r.Assets = append(r.Assets, r.Assets[0]) },
	} {
		r := completeCarRelease("0.2.17-car.7")
		mutate(r)
		svc := NewUpdateService(&updateServiceCacheStub{}, &updateServiceGitHubClientStub{release: r}, "0.2.17-car.6", "release")
		info, err := svc.CheckUpdate(context.Background(), true)
		require.NoError(t, err)
		require.False(t, info.HasUpdate)
		require.NotEmpty(t, info.Warning)
	}
}

func TestCarUpdateNeverFallsBackToBinaryReplacement(t *testing.T) {
	svc := NewUpdateService(nil, nil, "0.2.17-car.6", "release")
	require.ErrorIs(t, svc.PerformUpdate(context.Background()), ErrManagedUpdateRequired)
	require.ErrorIs(t, svc.Rollback(), ErrManagedUpdateRequired)
	require.ErrorIs(t, svc.RollbackToVersion(context.Background(), "0.2.17-car.5"), ErrManagedUpdateRequired)
	_, err := svc.PrepareManagedUpdate(context.Background(), "sysop-test")
	require.ErrorIs(t, err, ErrManagedUpdaterUnavailable)
	_, err = svc.ActivateManagedUpdate(context.Background(), "sysop-test")
	require.ErrorIs(t, err, ErrManagedUpdaterUnavailable)
}

func unixUpdaterFixture(t *testing.T, handler http.HandlerFunc) *UnixManagedUpdater {
	t.Helper()
	path := filepath.Join(t.TempDir(), "updater.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return NewUnixManagedUpdater(path)
}

func TestUnixManagedUpdaterProtocol(t *testing.T) {
	var paths []string
	client := unixUpdaterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method == http.MethodPost {
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "sysop-test", body["operation_id"])
			if r.URL.Path == "/v1/prepare" {
				require.Equal(t, "0.2.17-car.7", body["version"])
			}
			w.WriteHeader(http.StatusAccepted)
		}
		_ = json.NewEncoder(w).Encode(ManagedUpdateStatus{ProtocolVersion: 1, Phase: "ready", OperationID: "sysop-test", TargetVersion: "0.2.17-car.7", TargetCommit: strings.Repeat("a", 40)})
	})
	for _, run := range []func() (*ManagedUpdateStatus, error){
		func() (*ManagedUpdateStatus, error) { return client.Status(context.Background()) },
		func() (*ManagedUpdateStatus, error) {
			return client.Prepare(context.Background(), "sysop-test", "0.2.17-car.7")
		},
		func() (*ManagedUpdateStatus, error) { return client.Activate(context.Background(), "sysop-test") },
	} {
		status, err := run()
		require.NoError(t, err)
		require.Equal(t, "ready", status.Phase)
	}
	require.Equal(t, []string{"/v1/status", "/v1/prepare", "/v1/activate"}, paths)
}

func TestUnixManagedUpdaterRejectsInvalidResponses(t *testing.T) {
	for _, body := range []string{
		`{"protocol_version":2,"phase":"idle"}`,
		`{"protocol_version":1,"phase":"unknown"}`,
		`{"protocol_version":1,"phase":"ready","operation_id":"sysop-test","target_version":"0.2.17-car.7"}`,
		`{"protocol_version":1,"phase":"idle"} {}`,
		strings.Repeat(" ", 16385),
	} {
		client := unixUpdaterFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		_, err := client.Status(context.Background())
		require.ErrorIs(t, err, ErrManagedUpdaterUnavailable)
	}
}

func TestUnixManagedUpdaterUnavailableAndCancelled(t *testing.T) {
	client := NewUnixManagedUpdater(filepath.Join(t.TempDir(), "missing.sock"))
	_, err := client.Status(context.Background())
	require.ErrorIs(t, err, ErrManagedUpdaterUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Status(ctx)
	require.ErrorIs(t, err, ErrManagedUpdaterUnavailable)
	require.Nil(t, NewUnixManagedUpdater("relative.sock"))
}
