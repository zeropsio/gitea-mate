package zerops_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

// deployFake builds a fake with one identity and one service that has never
// deployed.
func deployFake(t *testing.T) (*zeropstest.Fake, *zerops.Client) {
	t.Helper()
	f := zeropstest.New(t, "org-1")
	f.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: "org-1"})
	f.SetServices("prj-stage",
		zerops.Service{ID: "svc-api", ProjectID: "prj-stage", Name: "api", Status: "ACTIVE",
			Ports: []zerops.ServicePort{{Port: 3000, HTTPRouting: true}}},
		zerops.Service{ID: "svc-db", ProjectID: "prj-stage", Name: "db", Status: "ACTIVE"},
	)
	return f, f.Client("broker")
}

func TestAppVersionSha(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"a stage version is the bare sha", "3f9c1b2e", "3f9c1b2e"},
		{"a production version names the tag and the tagger too", "3f9c1b2e v1.2.0 u-abc", "3f9c1b2e"},
		{"a version nobody named has no sha", "", ""},
		{"a name that starts with a space has none either", " v1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (zerops.AppVersion{Name: tc.in}).Sha(); got != tc.want {
				t.Fatalf("Sha(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDeployOneArchive(t *testing.T) {
	t.Parallel()
	f, client := deployFake(t)
	ctx := context.Background()

	version, err := client.CreateAppVersion(ctx, "svc-api", "3f9c1b2e")
	if err != nil {
		t.Fatalf("CreateAppVersion: %v", err)
	}
	if version.ID == "" || version.Name != "3f9c1b2e" {
		t.Fatalf("CreateAppVersion = %+v, want an id and the name back", version)
	}

	archive := []byte("a tar.gz would go here")
	if err := client.UploadAppVersion(ctx, version.ID, archive); err != nil {
		t.Fatalf("UploadAppVersion: %v", err)
	}

	proc, err := client.BuildAndDeploy(ctx, version.ID, "zerops:\n  - setup: api\n", "api")
	if err != nil {
		t.Fatalf("BuildAndDeploy: %v", err)
	}
	final, err := client.AwaitProcess(ctx, proc.ID, time.Millisecond)
	if err != nil {
		t.Fatalf("AwaitProcess: %v", err)
	}
	if final.Status != zerops.ProcessFinished {
		t.Fatalf("process status = %q, want FINISHED", final.Status)
	}

	active, found, err := client.ActiveAppVersion(ctx, "svc-api")
	if err != nil || !found {
		t.Fatalf("ActiveAppVersion = %v, %v, %v", active, found, err)
	}
	if active.Sha() != "3f9c1b2e" {
		t.Fatalf("the live sha is %q, want 3f9c1b2e", active.Sha())
	}

	recorded := f.AppVersions("svc-api")
	if len(recorded) != 1 || !bytes.Equal(recorded[0].Archive, archive) {
		t.Fatalf("the platform holds %d versions, archive %q", len(recorded), recorded[0].Archive)
	}
	if recorded[0].Setup != "api" {
		t.Fatalf("setup = %q, want api", recorded[0].Setup)
	}
}

func TestBuildAndDeployNeedsBothYamlAndSetup(t *testing.T) {
	t.Parallel()
	_, client := deployFake(t)
	ctx := context.Background()

	version, err := client.CreateAppVersion(ctx, "svc-api", "3f9c")
	if err != nil {
		t.Fatalf("CreateAppVersion: %v", err)
	}
	if err := client.UploadAppVersion(ctx, version.ID, []byte("x")); err != nil {
		t.Fatalf("UploadAppVersion: %v", err)
	}
	if _, err := client.BuildAndDeploy(ctx, version.ID, "zerops: []", ""); zerops.Code(err) != "zeropsYamlSetupNotFound" {
		t.Fatalf("BuildAndDeploy with no setup = %v, want zeropsYamlSetupNotFound", err)
	}
}

func TestFailedBuildIsReportedNotHidden(t *testing.T) {
	t.Parallel()
	f, client := deployFake(t)
	ctx := context.Background()
	f.FailBuild("bad0c0de")

	version, err := client.CreateAppVersion(ctx, "svc-api", "bad0c0de")
	if err != nil {
		t.Fatalf("CreateAppVersion: %v", err)
	}
	if err := client.UploadAppVersion(ctx, version.ID, []byte("x")); err != nil {
		t.Fatalf("UploadAppVersion: %v", err)
	}
	proc, err := client.BuildAndDeploy(ctx, version.ID, "zerops: []", "api")
	if err != nil {
		t.Fatalf("BuildAndDeploy: %v", err)
	}
	final, err := client.AwaitProcess(ctx, proc.ID, time.Millisecond)
	if err != nil {
		t.Fatalf("AwaitProcess: %v", err)
	}
	if final.Status != zerops.ProcessFailed {
		t.Fatalf("process status = %q, want FAILED", final.Status)
	}
	if _, found, _ := client.ActiveAppVersion(ctx, "svc-api"); found {
		t.Fatal("a failed build left an ACTIVE version behind")
	}
}

func TestPromoteReadsAppCodeAndUploadsItElsewhere(t *testing.T) {
	t.Parallel()
	f, client := deployFake(t)
	f.SetServices("prj-prod", zerops.Service{ID: "svc-api-prod", ProjectID: "prj-prod", Name: "api",
		Ports: []zerops.ServicePort{{Port: 3000, HTTPRouting: true}}})
	ctx := context.Background()

	stage, err := client.CreateAppVersion(ctx, "svc-api", "3f9c")
	if err != nil {
		t.Fatalf("CreateAppVersion: %v", err)
	}
	archive := []byte("the built archive")
	if err := client.UploadAppVersion(ctx, stage.ID, archive); err != nil {
		t.Fatalf("UploadAppVersion: %v", err)
	}
	if _, err := client.BuildAndDeploy(ctx, stage.ID, "zerops: []", "api"); err != nil {
		t.Fatalf("BuildAndDeploy: %v", err)
	}

	url, err := client.AppCodeURL(ctx, stage.ID)
	if err != nil {
		t.Fatalf("AppCodeURL: %v", err)
	}
	bytesBack, err := client.Download(ctx, url)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !bytes.Equal(bytesBack, archive) {
		t.Fatalf("the promoted bytes are %q, want %q", bytesBack, archive)
	}

	prod, err := client.CreateAppVersion(ctx, "svc-api-prod", "3f9c v1.0.0 u-abc")
	if err != nil {
		t.Fatalf("CreateAppVersion: %v", err)
	}
	if err := client.UploadAppVersion(ctx, prod.ID, bytesBack); err != nil {
		t.Fatalf("UploadAppVersion: %v", err)
	}
	if _, err := client.BuildAndDeploy(ctx, prod.ID, "zerops: []", "api"); err != nil {
		t.Fatalf("BuildAndDeploy: %v", err)
	}
	active, found, err := client.ActiveAppVersion(ctx, "svc-api-prod")
	if err != nil || !found {
		t.Fatalf("ActiveAppVersion = %v %v %v", active, found, err)
	}
	if active.Sha() != "3f9c" || active.Name != "3f9c v1.0.0 u-abc" {
		t.Fatalf("production's version is %q", active.Name)
	}
}

func TestEnableSubdomainAccessIsAPostDeployCall(t *testing.T) {
	t.Parallel()
	f, client := deployFake(t)
	ctx := context.Background()

	// Measured three times: before a service has code the platform refuses.
	if err := client.EnableSubdomainAccess(ctx, "svc-api"); zerops.Status(err) != 400 {
		t.Fatalf("enable-subdomain-access before a deploy = %v, want 400", err)
	}
	if f.SubdomainEnabled("svc-api") {
		t.Fatal("the refusal still turned the subdomain on")
	}

	version, err := client.CreateAppVersion(ctx, "svc-api", "3f9c")
	if err != nil {
		t.Fatalf("CreateAppVersion: %v", err)
	}
	if err := client.UploadAppVersion(ctx, version.ID, []byte("x")); err != nil {
		t.Fatalf("UploadAppVersion: %v", err)
	}
	if _, err := client.BuildAndDeploy(ctx, version.ID, "zerops: []", "api"); err != nil {
		t.Fatalf("BuildAndDeploy: %v", err)
	}
	if err := client.EnableSubdomainAccess(ctx, "svc-api"); err != nil {
		t.Fatalf("enable-subdomain-access after a deploy: %v", err)
	}
	if !f.SubdomainEnabled("svc-api") {
		t.Fatal("the subdomain is still off after a 200")
	}
}

func TestAwaitProcessStopsWithTheContext(t *testing.T) {
	t.Parallel()
	f, client := deployFake(t)
	f.AddAppVersion(zerops.AppVersion{ID: "ver-stuck", Name: "3f9c", ServiceStackID: "svc-api", Status: "DEPLOYING"})
	f.AddProcess(zerops.Process{ID: "proc-stuck", Status: "RUNNING", ServiceStackID: "svc-api"})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := client.AwaitProcess(ctx, "proc-stuck", time.Millisecond)
	var timeout *zerops.ErrProcessTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("AwaitProcess on a stuck process = %v, want an ErrProcessTimeout", err)
	}
}

func TestServiceHTTP(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		service zerops.Service
		want    bool
	}{
		{"a web service routes http", zerops.Service{Ports: []zerops.ServicePort{{Port: 3000, HTTPRouting: true}}}, true},
		{"a database does not", zerops.Service{Ports: []zerops.ServicePort{{Port: 5432}}}, false},
		{"a service with no ports does not", zerops.Service{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.service.HTTP(); got != tc.want {
				t.Fatalf("HTTP() = %v, want %v", got, tc.want)
			}
		})
	}
}
