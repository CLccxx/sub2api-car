package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const carGitHubRepo = "CLccxx/sub2api-car"

var (
	carVersionPattern            = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)-car\.([0-9]+)$`)
	operationIDPattern           = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,160}$`)
	commitPattern                = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	ErrManagedUpdateRequired     = infraerrors.Conflict("MANAGED_UPDATE_REQUIRED", "This installation uses the host updater; binary replacement and manual downgrade are disabled")
	ErrManagedUpdaterUnavailable = infraerrors.ServiceUnavailable("MANAGED_UPDATER_UNAVAILABLE", "Host updater is unavailable; complete the one-time web updater installation or check its service")
)

type ManagedUpdateStatus struct {
	ProtocolVersion int    `json:"protocol_version"`
	Phase           string `json:"phase"`
	OperationID     string `json:"operation_id"`
	TargetVersion   string `json:"target_version"`
	TargetCommit    string `json:"target_commit"`
	CurrentVersion  string `json:"current_version,omitempty"`
	Message         string `json:"message"`
	ErrorCode       string `json:"error_code"`
	BackupPath      string `json:"backup_path"`
	Step            string `json:"step,omitempty"`
}

type ManagedUpdater interface {
	Status(context.Context) (*ManagedUpdateStatus, error)
	Prepare(context.Context, string, string) (*ManagedUpdateStatus, error)
	Activate(context.Context, string) (*ManagedUpdateStatus, error)
}

type UpdateServiceOptions struct{ ManagedUpdater ManagedUpdater }

func carVersionParts(version string) ([4]uint64, bool) {
	var result [4]uint64
	parts := carVersionPattern.FindStringSubmatch(version)
	if len(parts) != 5 {
		return result, false
	}
	for i := range result {
		n, err := strconv.ParseUint(parts[i+1], 10, 63)
		if err != nil {
			return result, false
		}
		result[i] = n
	}
	return result, true
}

func (s *UpdateService) IsManagedUpdate() bool {
	_, ok := carVersionParts(s.currentVersion)
	return ok
}

func (s *UpdateService) updateMode() string {
	if s.IsManagedUpdate() {
		return "managed"
	}
	return "binary"
}

func validateCarRelease(release *GitHubRelease) error {
	_, valid := carVersionParts(release.TagName)
	if !valid || !strings.HasPrefix(release.TagName, "v") || release.Draft || release.Prerelease {
		return fmt.Errorf("latest release is not a published carpool version")
	}
	version := strings.TrimPrefix(release.TagName, "v")
	name := "sub2api-car-" + version + "-linux-amd64.tar.gz"
	for _, expected := range []string{name, name + ".sha256"} {
		found := 0
		for _, asset := range release.Assets {
			if asset.Name != expected {
				continue
			}
			found++
			limit := int64(maxDownloadSize)
			if strings.HasSuffix(expected, ".sha256") {
				limit = 8192
			}
			if asset.Size <= 0 || asset.Size > limit || asset.BrowserDownloadURL != "https://github.com/"+carGitHubRepo+"/releases/download/"+release.TagName+"/"+expected {
				return fmt.Errorf("latest carpool release has an invalid deployment asset")
			}
		}
		if found != 1 {
			return fmt.Errorf("latest carpool release is missing its complete deployment bundle or checksum")
		}
	}
	return nil
}

func (s *UpdateService) GetManagedUpdateStatus(ctx context.Context) (*ManagedUpdateStatus, error) {
	if !s.IsManagedUpdate() || s.managedUpdater == nil {
		return nil, ErrManagedUpdaterUnavailable
	}
	status, err := s.managedUpdater.Status(ctx)
	return s.withCurrentVersion(status, err)
}

func (s *UpdateService) withCurrentVersion(status *ManagedUpdateStatus, err error) (*ManagedUpdateStatus, error) {
	if err != nil {
		return nil, err
	}
	if status == nil {
		return nil, ErrManagedUpdaterUnavailable
	}
	copy := *status
	copy.CurrentVersion = s.currentVersion
	return &copy, nil
}

func (s *UpdateService) PrepareManagedUpdate(ctx context.Context, operationID string) (*ManagedUpdateStatus, error) {
	if !s.IsManagedUpdate() || s.managedUpdater == nil {
		return nil, ErrManagedUpdaterUnavailable
	}
	info, err := s.CheckUpdate(ctx, true)
	if err != nil {
		return nil, err
	}
	if info.Warning != "" {
		return nil, infraerrors.ServiceUnavailable("UPDATE_CHECK_FAILED", "Unable to verify the latest carpool release; refresh and try again")
	}
	if !info.HasUpdate {
		return nil, ErrNoUpdateAvailable
	}
	status, err := s.managedUpdater.Prepare(ctx, operationID, info.LatestVersion)
	return s.withCurrentVersion(status, err)
}

func (s *UpdateService) ActivateManagedUpdate(ctx context.Context, operationID string) (*ManagedUpdateStatus, error) {
	if !s.IsManagedUpdate() || s.managedUpdater == nil {
		return nil, ErrManagedUpdaterUnavailable
	}
	if !operationIDPattern.MatchString(operationID) {
		return nil, infraerrors.BadRequest("INVALID_UPDATE_OPERATION", "A prepared update operation is required")
	}
	status, err := s.managedUpdater.Activate(ctx, operationID)
	return s.withCurrentVersion(status, err)
}

// The app can request only the fixed updater protocol over a local Unix socket.
// It receives no Docker socket, host command execution, or client-supplied URLs.
type UnixManagedUpdater struct{ client *http.Client }

func NewUnixManagedUpdater(socket string) *UnixManagedUpdater {
	if socket == "" || !filepath.IsAbs(socket) {
		return nil
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}
	return &UnixManagedUpdater{client: &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *UnixManagedUpdater) Status(ctx context.Context) (*ManagedUpdateStatus, error) {
	return c.request(ctx, http.MethodGet, "/v1/status", nil)
}
func (c *UnixManagedUpdater) Prepare(ctx context.Context, id, version string) (*ManagedUpdateStatus, error) {
	return c.request(ctx, http.MethodPost, "/v1/prepare", map[string]string{"operation_id": id, "version": version})
}
func (c *UnixManagedUpdater) Activate(ctx context.Context, id string) (*ManagedUpdateStatus, error) {
	return c.request(ctx, http.MethodPost, "/v1/activate", map[string]string{"operation_id": id})
}

func (c *UnixManagedUpdater) request(ctx context.Context, method, path string, payload any) (*ManagedUpdateStatus, error) {
	if c == nil {
		return nil, ErrManagedUpdaterUnavailable
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, ErrManagedUpdaterUnavailable
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://updater"+path, body)
	if err != nil {
		return nil, ErrManagedUpdaterUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, ErrManagedUpdaterUnavailable
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16385))
	if err != nil || len(data) > 16384 {
		return nil, ErrManagedUpdaterUnavailable
	}
	if resp.StatusCode == http.StatusConflict {
		return nil, infraerrors.Conflict("UPDATE_OPERATION_CONFLICT", "Another update is active or the prepared operation has changed; refresh its status")
	}
	if resp.StatusCode == http.StatusBadRequest {
		return nil, infraerrors.BadRequest("UPDATE_OPERATION_INVALID", "The updater rejected this operation; refresh its status")
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, ErrManagedUpdaterUnavailable
	}
	var status ManagedUpdateStatus
	if json.Unmarshal(data, &status) != nil || !validManagedStatus(&status) {
		return nil, ErrManagedUpdaterUnavailable
	}
	return &status, nil
}

func validManagedStatus(status *ManagedUpdateStatus) bool {
	if status.ProtocolVersion != 1 || len(status.Message) > 1024 || len(status.ErrorCode) > 128 || len(status.BackupPath) > 1024 || len(status.Step) > 128 {
		return false
	}
	switch status.Phase {
	case "idle":
		return status.OperationID == "" && status.TargetVersion == ""
	case "preparing", "ready", "applying", "succeeded", "failed", "rolled_back", "manual_recovery_required":
	default:
		return false
	}
	if !operationIDPattern.MatchString(status.OperationID) {
		return false
	}
	if _, ok := carVersionParts(status.TargetVersion); !ok {
		return false
	}
	if status.TargetCommit != "" && !commitPattern.MatchString(status.TargetCommit) {
		return false
	}
	if status.Phase == "ready" || status.Phase == "applying" || status.Phase == "succeeded" || status.Phase == "rolled_back" {
		return commitPattern.MatchString(status.TargetCommit)
	}
	return true
}
