package mirror_test

import (
	"testing"

	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// The press (pass 28) is the last thing a browser does for a Mate: it creates
// the project, imports zcp with createIntegrationToken false and its own key
// as the secret ZCP_API_KEY, the tier's runtimes plan as MATE_SETUP_RUNTIMES,
// and writes the registry tag. Nothing after it — no key lowered, no close-off,
// no restart, no later tag. The first pass that reads the tag serves the Mate
// from that state alone, on a container that is not running yet, and leaves
// what the press wrote as it found it.
func TestAMateIsServedFromWhatThePressLeftAlone(t *testing.T) {
	statuses := []string{"NEW", "READY_TO_DEPLOY", "ACTIVE"}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			r := newRig(t)
			r.zerops.SetServices("p-fen",
				zerops.Service{ID: zcpService, ProjectID: "p-fen", Name: "zcp", Status: status,
					TypeInfo: zerops.ServiceTypeInfo{VersionName: "zcp@1"}},
			)
			pressed := []zerops.ServiceUserData{
				{Key: "ZCP_API_KEY", Content: "key-" + "minted-at-the-press", Sensitive: true},
				{Key: "MATE_SETUP_RUNTIMES", Content: "c2VydmljZXM6IFtd"},
			}
			r.zerops.SetUserData(zcpService, pressed...)

			result := r.passAt(t, now)
			if len(result.Failures) != 0 || len(result.Problems) != 0 {
				t.Fatalf("failures %v, problems %v", result.Failures, result.Problems)
			}
			vars := r.vars()
			if vars[mirror.VarGiteaURL].Content != giteaPublicURL || vars[mirror.VarBrokerURL].Content != brokerPublicURL ||
				vars[mirror.VarGiteaToken].Content == "" {
				t.Errorf("the Git variables were not delivered: %+v", vars)
			}
			for _, want := range pressed {
				if got := vars[want.Key]; got.Content != want.Content || got.Sensitive != want.Sensitive {
					t.Errorf("%s = %+v, want what the press wrote", want.Key, got)
				}
			}
			if _, updates, restarts := r.touchedContainer(); updates != 0 || restarts != 0 {
				t.Errorf("updates %d restarts %d; a delivery adds its three and moves nothing", updates, restarts)
			}
		})
	}
}
