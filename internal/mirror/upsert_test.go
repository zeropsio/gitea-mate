package mirror_test

import (
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// A delivery is an upsert: a variable the container holds is updated by its
// id, whatever its case and whether the first reads listed it or not, and only
// a variable the platform does not hold is created. The platform refuses a
// second variable of a key in any case (400 userDataDuplicateKey, measured
// 2026-09-30, when three Mates' rotations failed every pass for seven hours).
func TestADeliveryUpdatesWhatTheContainerHoldsAndCreatesOnlyWhatItLacks(t *testing.T) {
	held := "value-1"
	cases := []struct {
		name string
		// arrange gives Fen's container what it holds before the pass.
		arrange func(r *rig)
		// creates is the most creates the pass may make: one refused create
		// is how a short read is found out, and a variable under another
		// case is deleted and created anew.
		creates int
	}{
		{
			name: "a rotation whose variables the pass's reads leave out",
			arrange: func(r *rig) {
				r.zerops.SetUserData(zcpService,
					zerops.ServiceUserData{Key: mirror.VarGiteaURL, Content: giteaPublicURL},
					zerops.ServiceUserData{Key: mirror.VarBrokerURL, Content: brokerPublicURL},
					zerops.ServiceUserData{Key: mirror.VarGiteaToken, Content: held, Sensitive: true},
				)
				// The gather's read and the delivery's own: both short.
				r.zerops.HideUserData(zcpService, 2, mirror.VarGiteaURL, mirror.VarBrokerURL, mirror.VarGiteaToken)
			},
			creates: 1,
		},
		{
			name: "a token the pass's reads leave out, the plain variables listed",
			arrange: func(r *rig) {
				r.zerops.SetUserData(zcpService,
					zerops.ServiceUserData{Key: mirror.VarGiteaURL, Content: giteaPublicURL},
					zerops.ServiceUserData{Key: mirror.VarBrokerURL, Content: brokerPublicURL},
					zerops.ServiceUserData{Key: mirror.VarGiteaToken, Content: held, Sensitive: true},
				)
				r.zerops.HideUserData(zcpService, 2, mirror.VarGiteaToken)
			},
			creates: 1,
		},
		{
			name: "variables held under another case",
			arrange: func(r *rig) {
				r.zerops.SetUserData(zcpService,
					zerops.ServiceUserData{Key: strings.ToLower(mirror.VarGiteaURL), Content: "https://elsewhere.example"},
					zerops.ServiceUserData{Key: "Mate_Broker_Url", Content: brokerPublicURL},
					zerops.ServiceUserData{Key: strings.ToLower(mirror.VarGiteaToken), Content: held, Sensitive: true},
				)
			},
			creates: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.gitea.AddUser(gitea.User{Login: "mate-p-fen", Active: true, Restricted: true})
			r.gitea.AddToken("mate-p-fen", mirror.TokenName("mate-p-fen", 1), held, mirror.BotScopes...)
			tc.arrange(r)

			result := r.passAt(t, now)
			if len(result.Failures) != 0 {
				t.Fatalf("failures = %v", result.Failures)
			}

			entries := r.zerops.UserData(zcpService)
			if len(entries) != 3 {
				t.Fatalf("the container holds %d variables, want the three: %+v", len(entries), entries)
			}
			vars := r.vars()
			if got := vars[mirror.VarGiteaURL].Content; got != giteaPublicURL {
				t.Errorf("GITEA_URL = %q", got)
			}
			if got := vars[mirror.VarBrokerURL].Content; got != brokerPublicURL {
				t.Errorf("MATE_BROKER_URL = %q", got)
			}
			newest := r.gitea.Tokens("mate-p-fen")
			token := vars[mirror.VarGiteaToken].Content
			if token == "" || !strings.HasSuffix(token, newest[len(newest)-1]) {
				t.Errorf("GITEA_TOKEN = %q, want the bot's newest generation of %v", token, newest)
			}
			if creates, _, _ := r.touchedContainer(); creates > tc.creates {
				t.Errorf("%d creates on a container that held every variable, want at most %d", creates, tc.creates)
			}
		})
	}
}

// A variable no read ever lists cannot be updated, since the update needs its
// id: the delivery stops at the refusal, before a mint, and the next pass
// tries again — no generation is left behind for a token nobody was given.
func TestAVariableNoReadListsStopsTheDeliveryBeforeAMint(t *testing.T) {
	r := newRig(t)
	r.zerops.SetUserData(zcpService,
		zerops.ServiceUserData{Key: mirror.VarGiteaURL, Content: giteaPublicURL},
	)
	r.zerops.HideUserData(zcpService, 1000, mirror.VarGiteaURL)

	result := r.passAt(t, now)
	if len(result.Failures) != 1 || !strings.Contains(result.Failures[0], mirror.VarGiteaURL) {
		t.Fatalf("failures = %v, want the GITEA_URL write", result.Failures)
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 0 {
		t.Errorf("tokens = %v, want none", got)
	}
}

// A variable held under another case is replaced — deleted by its id, then
// created under its own name — never renamed by an update, whose renaming
// nobody has measured. It costs no mint: a token the container holds is
// moved as it is, and the next pass finds nothing to do.
func TestAVariableHeldUnderAnotherCaseIsReplacedWithoutAMint(t *testing.T) {
	r := newRig(t)
	held := "value-1"
	r.gitea.AddUser(gitea.User{Login: "mate-p-fen", FullName: "Fen", Active: true, Restricted: true})
	r.gitea.AddToken("mate-p-fen", mirror.TokenName("mate-p-fen", 1), held, mirror.BotScopes...)
	r.zerops.SetUserData(zcpService,
		zerops.ServiceUserData{Key: strings.ToLower(mirror.VarGiteaURL), Content: giteaPublicURL},
		zerops.ServiceUserData{Key: "Mate_Broker_Url", Content: brokerPublicURL},
		zerops.ServiceUserData{Key: strings.ToLower(mirror.VarGiteaToken), Content: held, Sensitive: true},
	)

	if result := r.passAt(t, now); len(result.Failures) != 0 {
		t.Fatalf("failures = %v", result.Failures)
	}
	vars := r.vars()
	for key, want := range map[string]string{
		mirror.VarGiteaURL: giteaPublicURL, mirror.VarBrokerURL: brokerPublicURL, mirror.VarGiteaToken: held,
	} {
		if got := vars[key]; got.Content != want {
			t.Errorf("%s = %+v, want %q under its own name", key, got, want)
		}
	}
	if !vars[mirror.VarGiteaToken].Sensitive {
		t.Error("the token was moved out of the sensitive variables")
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 1 {
		t.Errorf("tokens = %v, want generation 1 alone: a case is no reason to mint", got)
	}
	if second := r.passAt(t, now.Add(mirror.DefaultInterval)); second.Planned != 0 {
		t.Errorf("the second pass plans:\n%s", mirror.Describe(second.Plan))
	}
}

// A replacement the platform refuses is tried once a pass, and never mints.
func TestARefusedReplacementIsTriedOnceAPassAndNeverMints(t *testing.T) {
	r := newRig(t)
	held := "value-1"
	r.gitea.AddUser(gitea.User{Login: "mate-p-fen", FullName: "Fen", Active: true, Restricted: true})
	r.gitea.AddToken("mate-p-fen", mirror.TokenName("mate-p-fen", 1), held, mirror.BotScopes...)
	r.zerops.SetUserData(zcpService,
		zerops.ServiceUserData{ID: "ud-url", Key: strings.ToLower(mirror.VarGiteaURL), Content: giteaPublicURL},
		zerops.ServiceUserData{Key: mirror.VarBrokerURL, Content: brokerPublicURL},
		zerops.ServiceUserData{Key: mirror.VarGiteaToken, Content: held, Sensitive: true},
	)
	r.zerops.Fail["DELETE /user-data/ud-url"] = 500

	for i := range 3 {
		r.zerops.Requests = nil
		r.passAt(t, now.Add(time.Duration(i)*mirror.DefaultInterval))
		deletes := 0
		for _, req := range r.zerops.Requests {
			if strings.HasPrefix(req, "DELETE /user-data/") {
				deletes++
			}
		}
		if deletes != 1 {
			t.Errorf("pass %d: %d deletes, want one attempt", i, deletes)
		}
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 1 {
		t.Errorf("tokens = %v, want generation 1 alone", got)
	}
}
