package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
	"github.com/zeropsio/gitea-mate/internal/pipeline"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

// One account: an org with a Gitea project carrying the registry, a group
// `acme` with a stage and a production project, and one service `api`.
const (
	orgID     = "org-1"
	giteaPrj  = "prj-gitea"
	stagePrj  = "prj-stage"
	prodPrj   = "prj-prod"
	matePrj   = "prj-mate"
	groupID   = "g-1"
	ownerUser = "user-owner"
	plainUser = "user-plain"
)

const first = "1111111111111111111111111111111111111111"
const second = "2222222222222222222222222222222222222222"

const envFile = `
version: 1
environments:
  stage:
    tier: stage
    project: prj-stage
    sources: [main]
  production:
    tier: production
    project: prj-prod
    sources: release
`

const stageImport = `
services:
  - hostname: api
    type: nodejs@22
    buildFromGit: https://gitea.example/acme/api
    zeropsSetup: api
`

const productionImport = `
services:
  - hostname: api
    type: nodejs@22
    buildFromGit: https://gitea.example/acme/api
    zeropsSetup: api
`

const apiZeropsYaml = `
zerops:
  - setup: api
    build:
      base: nodejs@22
      buildCommands: [npm ci]
      deployFiles: ./
    run:
      base: nodejs@22
      start: npm start
`

type world struct {
	gitea  *giteatest.Fake
	zerops *zeropstest.Fake
	queue  *deploy.Queue
	pipe   *pipeline.Pipeline
}

func newWorld(t *testing.T) *world {
	t.Helper()

	g := giteatest.New(t)
	g.AddRepo("acme/group", "main")
	g.AddRepo("acme/api", "main")
	g.AddFile("acme/group", "main", "environments.yaml", envFile)
	g.AddFile("acme/group", "main", "3 — Stage/import.yaml", stageImport)
	g.AddFile("acme/group", "main", "4 — Small Production/import.yaml", productionImport)
	g.SetBranch("acme/api", "main", first)
	for _, sha := range []string{first, second} {
		g.AddFile("acme/api", sha, "zerops.yaml", apiZeropsYaml)
		g.SetArchive("acme/api", sha, []byte("archive of "+sha))
	}

	z := zeropstest.New(t, orgID)
	z.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: orgID})
	z.AddMember(zerops.Member{ID: "cu-owner", UserID: ownerUser, Status: "ACTIVE", RoleCode: "OWNER",
		User: zerops.UserLight{ID: ownerUser, Email: "owner@example.com", FullName: "The Owner"}})
	z.AddMember(zerops.Member{ID: "cu-plain", UserID: plainUser, Status: "ACTIVE", RoleCode: "READ_ONLY",
		User: zerops.UserLight{ID: plainUser, Email: "plain@example.com", FullName: "A Reader"}})
	z.SetProjects(
		zerops.Project{ID: giteaPrj, ClientID: orgID, Name: "Gitea", TagList: []string{
			"mate:tool:gitea",
			"mate:gn:" + groupID + ":acme",
			"mate:gm:" + groupID + ":" + matePrj + ":mate",
			"mate:gm:" + groupID + ":" + stagePrj + ":stage",
			"mate:gm:" + groupID + ":" + prodPrj + ":production",
		}},
		zerops.Project{ID: matePrj, ClientID: orgID, Name: "Mate"},
		zerops.Project{ID: stagePrj, ClientID: orgID, Name: "Stage"},
		zerops.Project{ID: prodPrj, ClientID: orgID, Name: "Production"},
	)
	z.SetServices(stagePrj, zerops.Service{ID: "svc-stage-api", ProjectID: stagePrj, Name: "api",
		Status: "ACTIVE", Ports: []zerops.ServicePort{{Port: 3000, HTTPRouting: true}}})
	z.SetServices(prodPrj, zerops.Service{ID: "svc-prod-api", ProjectID: prodPrj, Name: "api",
		Status: "ACTIVE", Ports: []zerops.ServicePort{{Port: 3000, HTTPRouting: true}}})
	z.SetServices(giteaPrj, zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"})

	client := g.Client()
	zclient := z.Client("broker")
	records := deploy.NewRecords(0)
	executor := &deploy.Executor{
		Zerops: zclient, Gitea: client, ClientID: orgID, Records: records,
		PollInterval: time.Millisecond, Timeout: 5 * time.Second,
	}
	queue := deploy.NewQueue(executor.Run, nil)
	return &world{
		gitea: g, zerops: z, queue: queue,
		pipe: &pipeline.Pipeline{
			Zerops: zclient, Gitea: client, ClientID: orgID, GiteaProjectID: giteaPrj,
			Resolver: &deploy.Resolver{Gitea: client, Merger: &deploy.Merger{Gitea: client}},
			Queue:    queue, Records: records,
		},
	}
}

func (w *world) statuses(t *testing.T, repo, sha string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, s := range w.gitea.Statuses(repo, sha) {
		out[s.Context] = s.State
	}
	return out
}

// tagDelivery is what Gitea posts when a `v*` tag is created.
func tagDelivery(tag, commit, pusher string) []byte {
	body := map[string]any{
		"ref": tag, "ref_type": "tag", "sha": commit,
		"repository": map[string]any{"full_name": "acme/group"},
		"sender":     map[string]any{"login": pusher, "username": pusher},
	}
	raw, _ := json.Marshal(body)
	return raw
}

func (w *world) tag(t *testing.T, name, commit, message string, when time.Time) {
	t.Helper()
	tag := gitea.Tag{Name: name, ID: "obj-" + name}
	tag.Commit.SHA = commit
	w.gitea.AddTag("acme/group", tag, gitea.AnnotatedTag{
		Tag: name, SHA: "obj-" + name, Message: message,
		Tagger: gitea.TagUser{Name: "u-" + name, Date: when},
	})
}

func TestAReleaseIsJudgedOnItsPusher(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		pusher   string
		switchOn bool
		want     string
	}{
		{"a person with production rights", roles.Login(ownerUser), false, deploy.ReleaseApproved},
		{"a person without them", roles.Login(plainUser), false, deploy.ReleaseRefused},
		{"a Mate's bot with the group's switch off", "mate-" + matePrj, false, deploy.ReleaseRefused},
		{"a Mate's bot with the group's switch on", "mate-" + matePrj, true, deploy.ReleaseApproved},
		{"a login this organisation does not carry", "u-stranger", false, deploy.ReleaseRefused},
		{"a bot of another group", "mate-prj-elsewhere", true, deploy.ReleaseRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			if tc.switchOn {
				project, _ := w.zerops.Project(giteaPrj)
				w.zerops.SetProjectTags(giteaPrj, append(project.TagList, "mate:release:"+groupID+":mates"))
			}
			w.tag(t, "v1.0.0", "commit-1", "api "+first+"\n", time.Now())

			if err := w.pipe.Create(context.Background(), "acme", tagDelivery("v1.0.0", "commit-1", tc.pusher)); err != nil {
				t.Fatalf("Create: %v", err)
			}
			w.queue.Wait()

			got := w.statuses(t, "acme/group", "commit-1")[deploy.ReleaseContext("v1.0.0")]
			if got != tc.want {
				t.Fatalf("mate/release/v1.0.0 = %q, want %q", got, tc.want)
			}
			deployed := len(w.zerops.AppVersions("svc-prod-api")) > 0
			if deployed != (tc.want == deploy.ReleaseApproved) {
				t.Fatalf("production deployed = %v, with the verdict %q", deployed, got)
			}
		})
	}
}

func TestARefusedTagStaysRefusedOnARedelivery(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.tag(t, "v1.0.0", "commit-1", "api "+first+"\n", time.Now())

	// Refused: a reader cannot release.
	if err := w.pipe.Create(ctx, "acme", tagDelivery("v1.0.0", "commit-1", roles.Login(plainUser))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The same tag is delivered again, this time claiming the owner pushed it.
	// The verdict is already written, and a refused tag deploys nothing, ever.
	if err := w.pipe.Create(ctx, "acme", tagDelivery("v1.0.0", "commit-1", roles.Login(ownerUser))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.queue.Wait()

	written := w.gitea.Statuses("acme/group", "commit-1")
	verdicts := 0
	for _, s := range written {
		if s.Context == deploy.ReleaseContext("v1.0.0") {
			verdicts++
			if s.State != deploy.ReleaseRefused {
				t.Fatalf("the verdict changed to %q on a redelivery", s.State)
			}
		}
	}
	if verdicts != 1 {
		t.Fatalf("%d verdicts were written, want one", verdicts)
	}
	if len(w.zerops.AppVersions("svc-prod-api")) != 0 {
		t.Fatal("a refused tag deployed")
	}
}

func TestOnlyATagOnTheGroupRepoIsARelease(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"a branch, not a tag", map[string]any{
			"ref": "feature/x", "ref_type": "branch", "sha": "commit-1",
			"repository": map[string]any{"full_name": "acme/group"},
			"sender":     map[string]any{"login": roles.Login(ownerUser)}}},
		{"a tag that is not a release", map[string]any{
			"ref": "nightly", "ref_type": "tag", "sha": "commit-1",
			"repository": map[string]any{"full_name": "acme/group"},
			"sender":     map[string]any{"login": roles.Login(ownerUser)}}},
		{"a v* tag on a service repository", map[string]any{
			"ref": "v1.0.0", "ref_type": "tag", "sha": "commit-1",
			"repository": map[string]any{"full_name": "acme/api"},
			"sender":     map[string]any{"login": roles.Login(ownerUser)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.body)
			if err := w.pipe.Create(ctx, "acme", raw); err != nil {
				t.Fatalf("Create: %v", err)
			}
		})
	}
	w.queue.Wait()
	if len(w.gitea.Statuses("acme/group", "commit-1")) != 0 {
		t.Fatal("something that is not a release was judged as one")
	}
}
