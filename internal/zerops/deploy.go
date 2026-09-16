package zerops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ---------------------------------------------------------------------------
// App versions
// ---------------------------------------------------------------------------

// AppVersion is one deploy of one service. Name is what the broker writes the
// commit into (docs/group-repo.md, "What is deployed"): the full sha, and for
// production "{sha} {tag} {tagger}". The sha is always the first token, which
// is what lets the catch-up pass compare a desired head with what is live —
// an app version carries no source ref of its own.
type AppVersion struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	ServiceStackID string    `json:"serviceStackId"`
	ProjectID      string    `json:"projectId"`
	Status         string    `json:"status"`
	Sequence       int       `json:"sequence"`
	Created        time.Time `json:"created"`
	// UploadURL comes back on a create. The broker uploads through
	// PUT /app-version/{id}/upload instead, which is the measured path.
	UploadURL string `json:"uploadUrl"`
}

// App version statuses the broker reasons about. The platform's enum is
// longer; these are the three ends of a deploy.
const (
	AppVersionActive      = "ACTIVE"
	AppVersionBuildFailed = "BUILD_FAILED"
	AppVersionDeployFail  = "DEPLOY_FAILED"
)

// Sha is the commit an app version was built from: the first token of its
// name. An app version the broker did not create has no sha.
func (v AppVersion) Sha() string {
	for i := 0; i < len(v.Name); i++ {
		if v.Name[i] == ' ' {
			return v.Name[:i]
		}
	}
	return v.Name
}

// CreateAppVersion is POST /service-stack/{id}/app-version. The name is the
// only thing the body carries, and it is how every later read knows which
// commit is live.
func (c *Client) CreateAppVersion(ctx context.Context, serviceID, name string) (AppVersion, error) {
	var out AppVersion
	_, err := c.do(ctx, "POST", "/service-stack/"+url.PathEscape(serviceID)+"/app-version",
		map[string]string{"name": name}, &out)
	return out, err
}

// UploadAppVersion is PUT /app-version/{id}/upload — the archive as
// application/octet-stream, exactly as measured (ledger 2026-09-15).
func (c *Client) UploadAppVersion(ctx context.Context, versionID string, archive []byte) error {
	_, err := c.doRaw(ctx, "PUT", "/app-version/"+url.PathEscape(versionID)+"/upload",
		"application/octet-stream", archive)
	return err
}

// BuildAndDeploy is PUT /app-version/{id}/build-and-deploy. Both the yaml text
// and the setup name must be sent: the platform does not reuse a stored yaml
// (zcp's R2 recovery learnt the same on PUT …/deploy).
func (c *Client) BuildAndDeploy(ctx context.Context, versionID, zeropsYaml, setup string) (Process, error) {
	var out Process
	_, err := c.do(ctx, "PUT", "/app-version/"+url.PathEscape(versionID)+"/build-and-deploy",
		map[string]string{"zeropsYaml": zeropsYaml, "zeropsYamlSetup": setup}, &out)
	return out, err
}

// AppVersions is GET /service-stack/{id}/app-version — the direct list, not
// the Elasticsearch search, so a version that has just settled is already
// there. Newest first by sequence.
func (c *Client) AppVersions(ctx context.Context, serviceID string) ([]AppVersion, error) {
	var page struct {
		List []AppVersion `json:"list"`
	}
	if _, err := c.do(ctx, "GET", "/service-stack/"+url.PathEscape(serviceID)+"/app-version", nil, &page); err != nil {
		return nil, err
	}
	out := page.List
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Sequence > out[i].Sequence {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// ActiveAppVersion is the service's ACTIVE version, if it has one. A service
// that has never deployed has none, which is not an error — it is what "the
// first deploy" means.
func (c *Client) ActiveAppVersion(ctx context.Context, serviceID string) (AppVersion, bool, error) {
	versions, err := c.AppVersions(ctx, serviceID)
	if err != nil {
		return AppVersion{}, false, err
	}
	for _, v := range versions {
		if v.Status == AppVersionActive {
			return v, true, nil
		}
	}
	return AppVersion{}, false, nil
}

// AppCodeURL is GET /app-version/{id}/app-code: a pre-signed URL to the bytes
// that were uploaded at deploy time. A promotion downloads it and uploads it
// to the other service (ledger 2026-09-15).
func (c *Client) AppCodeURL(ctx context.Context, versionID string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	_, err := c.do(ctx, "GET", "/app-version/"+url.PathEscape(versionID)+"/app-code", nil, &out)
	return out.URL, err
}

// Download fetches a pre-signed URL. It carries no Authorization header: the
// URL is the credential, and sending the broker's token to a storage host
// would be handing it somewhere it does not belong.
func (c *Client) Download(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("zerops download: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zerops download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Status: resp.StatusCode, Code: "download_failed", Message: "the app code could not be downloaded"}
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxArchive))
}

// maxArchive bounds what the broker moves between Gitea and Zerops in one
// deploy. A repository archive larger than this is a problem to report, not a
// buffer to grow.
const maxArchive = 512 << 20

// EnableSubdomainAccess is PUT /service-stack/{id}/enable-subdomain-access. It
// is a post-deploy call: before a service has code the platform answers 400,
// three times measured (ledger 2026-09-15/16).
func (c *Client) EnableSubdomainAccess(ctx context.Context, serviceID string) error {
	_, err := c.do(ctx, "PUT", "/service-stack/"+url.PathEscape(serviceID)+"/enable-subdomain-access", nil, nil)
	return err
}

// ---------------------------------------------------------------------------
// Processes
// ---------------------------------------------------------------------------

// Process is one platform job. A deploy is watched through it.
type Process struct {
	ID             string `json:"id"`
	ProjectID      string `json:"projectId"`
	ServiceStackID string `json:"serviceStackId"`
	Status         string `json:"status"`
	ActionName     string `json:"actionName"`
	AppVersion     *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"appVersion"`
}

// The process statuses that end one.
const (
	ProcessFinished = "FINISHED"
	ProcessFailed   = "FAILED"
	ProcessCanceled = "CANCELED"
)

// Done reports whether the process has stopped moving.
func (p Process) Done() bool {
	switch p.Status {
	case ProcessFinished, ProcessFailed, ProcessCanceled:
		return true
	}
	return false
}

// Process is GET /process/{id}.
func (c *Client) Process(ctx context.Context, processID string) (Process, error) {
	var out Process
	_, err := c.do(ctx, "GET", "/process/"+url.PathEscape(processID), nil, &out)
	return out, err
}

// DefaultPollInterval is how often a deploy asks the platform where it is.
const DefaultPollInterval = 3 * time.Second

// ErrProcessTimeout is returned by [Client.AwaitProcess] when the deadline
// passed before the platform finished. The deploy may still land; the broker
// records the timeout and the next catch-up pass reads what is true then.
type ErrProcessTimeout struct {
	ProcessID string
	Status    string
}

func (e *ErrProcessTimeout) Error() string {
	return fmt.Sprintf("process %s was still %s when the deadline passed", e.ProcessID, e.Status)
}

// AwaitProcess polls until the process ends or ctx does. Interval may be zero,
// which means [DefaultPollInterval]. A FAILED or CANCELED process is returned
// with no error — the caller decides what a failure means; only a read that
// could not happen is an error.
func (c *Client) AwaitProcess(ctx context.Context, processID string, interval time.Duration) (Process, error) {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	last := Process{ID: processID}
	for {
		proc, err := c.Process(ctx, processID)
		if err != nil {
			// A read the deadline cut short is the deadline, not a platform
			// refusal: the deploy may still land, and the next catch-up pass
			// reads what is true then.
			if ctx.Err() != nil {
				return last, &ErrProcessTimeout{ProcessID: processID, Status: last.Status}
			}
			return last, err
		}
		last = proc
		if proc.Done() {
			return proc, nil
		}
		select {
		case <-ctx.Done():
			return last, &ErrProcessTimeout{ProcessID: processID, Status: last.Status}
		case <-ticker.C:
		}
	}
}

// doRaw makes one call whose body is not JSON — the archive upload, and
// nothing else so far.
func (c *Client) doRaw(ctx context.Context, method, path, contentType string, body []byte) (time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+basePath+path, bytes.NewReader(body))
	if err != nil {
		return time.Time{}, fmt.Errorf("zerops api: %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))

	resp, err := c.http.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("zerops api: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	date := parseDate(resp.Header.Get("Date"))
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return date, fmt.Errorf("zerops api: %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, apiErr)
		if apiErr.Code == "" {
			apiErr.Code = "http_" + fmt.Sprint(resp.StatusCode)
		}
		return date, apiErr
	}
	return date, nil
}
