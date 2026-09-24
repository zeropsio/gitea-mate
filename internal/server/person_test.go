package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/config"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/throwaway"
	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

// peopleRig is the rig with the three dependencies POST /person/token needs:
// a throwaway checker over the fake Zerops, the rights read live from it, and
// a pass that counts how often the route asked for one.
type peopleRig struct {
	*rig
	passes atomic.Int32
}

func newPeopleRig(t *testing.T) *peopleRig {
	t.Helper()
	r := newRig(t)
	z := r.zerops
	// Jan's throwaway: no rights, no flags, named for this Gitea, thirty
	// seconds old, made by Jan. Gone's: the same shape, made by somebody the
	// org only invited.
	z.AddIdentity("throwaway-jan", zeropstest.Identity{UserInfoID: "tok-jan", TokenID: "tok-jan", ClientID: clientID})
	z.AddToken(zerops.Token{
		ID: "tok-jan", Name: "gitea-signin:" + giteaHost + ":n1",
		RoleCode: "NO_ACCESS", CreatedByUser: "u-jan", Created: now.Add(-30 * time.Second),
	})
	z.AddMember(zerops.Member{
		ID: "cu-gone", UserID: "u-gone", Status: "INVITED", RoleCode: "READ_ONLY",
		User: zerops.UserLight{ID: "u-gone", Email: "gone@example", FullName: "Gone"},
	})
	z.AddIdentity("throwaway-gone", zeropstest.Identity{UserInfoID: "tok-gone", TokenID: "tok-gone", ClientID: clientID})
	z.AddToken(zerops.Token{
		ID: "tok-gone", Name: "gitea-signin:" + giteaHost + ":n2",
		RoleCode: "NO_ACCESS", CreatedByUser: "u-gone", Created: now.Add(-30 * time.Second),
	})

	p := &peopleRig{rig: r}
	cfg := &config.Config{
		ZeropsClientID: clientID, ZeropsProjectID: giteaPrj,
		GiteaURL: r.gitea.URL(), GiteaPublicURL: "https://" + giteaHost,
		GiteaWebhookSecret: "the-webhook-secret",
		BrokerPublicURL:    "https://broker.example",
		MateAppURL:         "https://app.example",
		GiteaOIDCSourceID:  1,
		AppTokenTTL:        12 * time.Hour,
		RunnerQuietPeriod:  50 * time.Millisecond,
	}
	broker := z.Client("broker")
	s := New(cfg, slog.New(slog.DiscardHandler), Deps{
		Zerops: broker,
		Gitea:  r.gitea.Client(),
		Throwaway: &throwaway.Checker{
			Broker: broker, AsCaller: z.Client, ClientID: clientID, GiteaHost: giteaHost,
			Retry: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		},
		Rights: rightsFromTheFake(broker),
		Pass: func(ctx context.Context) error {
			p.passes.Add(1)
			return nil
		},
	})
	t.Cleanup(s.Close)
	p.server, p.handler = s, s.Handler()
	return p
}

type rightsFunc func(ctx context.Context, caller throwaway.Caller) (roles.Rights, error)

func (f rightsFunc) For(ctx context.Context, caller throwaway.Caller) (roles.Rights, error) {
	return f(ctx, caller)
}

// rightsFromTheFake is what main wires: the org read live, the role function
// on the caller.
func rightsFromTheFake(z *zerops.Client) RightsReader {
	return rightsFunc(func(ctx context.Context, caller throwaway.Caller) (roles.Rights, error) {
		org, err := mirror.ReadOrgWith(ctx, z, clientID, giteaPrj, caller.Members)
		if err != nil {
			return roles.Rights{}, err
		}
		rights, _ := org.RightsFor(caller.UserID)
		return rights, nil
	})
}

func (p *peopleRig) personToken(bearer, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/person/token", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return p.do(req)
}

func TestAPersonGetsATokenThatActsAsThemAndAnAccountBoundToTheSource(t *testing.T) {
	r := newPeopleRig(t)

	rr := r.personToken("throwaway-jan", "https://app.example")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /person/token = %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Token     string `json:"token"`
		Login     string `json:"login"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	login := roles.Login("u-jan")
	if body.Login != login || body.Token == "" || body.ExpiresIn != 12*60*60 {
		t.Fatalf("body = %+v, want login %s, a token and expiresIn 43200", body, login)
	}

	// The account: bound to the OIDC source under Jan's Zerops id, so a later
	// *Sign in with Zerops* on Gitea's own pages is the same account.
	user, ok := r.gitea.User(login)
	if !ok {
		t.Fatalf("Gitea has no account %s", login)
	}
	if user.SourceID != 1 || user.LoginName != "u-jan" || user.Email != "jan@example" || user.FullName != "Jan" {
		t.Errorf("account = %+v, want source 1, login_name u-jan, Jan's email and name", user)
	}
	if user.Restricted {
		t.Errorf("a person is not restricted")
	}

	// The token acts as Jan and is named as the loop will retire it.
	who, err := r.gitea.TokenOnly(body.Token).WhoAmI(context.Background())
	if err != nil || who.Login != login {
		t.Fatalf("WhoAmI with the minted token = %+v, %v; want %s", who, err, login)
	}
	names := r.gitea.Tokens(login)
	if len(names) != 1 || !strings.HasPrefix(names[0], mirror.AppTokenPrefix) {
		t.Errorf("Jan's tokens = %v, want one named %s…", names, mirror.AppTokenPrefix)
	}

	// A pass ran once the account existed, so Jan is in the teams before the
	// app's first read.
	if got := r.passes.Load(); got != 1 {
		t.Errorf("passes = %d, want 1", got)
	}

	// CORS: every origin, since the proof is the bearer (D22).
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Errorf("Access-Control-Allow-Headers = %q, want Authorization allowed", got)
	}
}

func TestASecondSignInReusesTheAccountAndMintsAnotherToken(t *testing.T) {
	r := newPeopleRig(t)
	first := r.personToken("throwaway-jan", "")
	second := r.personToken("throwaway-jan", "")
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("codes = %d, %d", first.Code, second.Code)
	}
	login := roles.Login("u-jan")
	if names := r.gitea.Tokens(login); len(names) != 2 || names[0] == names[1] {
		t.Errorf("Jan's tokens = %v, want two distinct", names)
	}
	// The account existed the second time, so no pass was asked for.
	if got := r.passes.Load(); got != 1 {
		t.Errorf("passes = %d, want 1", got)
	}
}

// The recipe's Gitea had no login source yet on the owner's run of 2026-09-17
// (admin-init.sh had left it "for a later boot"): Gitea answered every account
// creation with 422 "login source does not exist [id: 1]", the broker turned
// that into a 502, the platform's edge replaced the 502 with its own page, and
// the app retried "still setting up" every twenty seconds for a quarter of an
// hour. A refusal is answered as one, in Gitea's words.
func TestAGiteaRefusalIsAnsweredInItsWordsNotAsStillSettingUp(t *testing.T) {
	r := newPeopleRig(t)
	r.server.cfg.GiteaOIDCSourceID = 9 // a source this Gitea does not have

	rr := r.personToken("throwaway-jan", "https://app.example")
	if rr.Code != http.StatusFailedDependency {
		t.Fatalf("POST /person/token = %d, want 424: %s", rr.Code, rr.Body.String())
	}
	var body struct{ Error, Message string }
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Error != "gitea_refused" || !strings.Contains(body.Message, "login source does not exist [id: 9]") {
		t.Errorf("body = %+v, want gitea_refused with Gitea's words", body)
	}
	if _, ok := r.gitea.User(roles.Login("u-jan")); ok {
		t.Errorf("an account was made despite the refusal")
	}
	if got := r.passes.Load(); got != 0 {
		t.Errorf("passes = %d, want 0", got)
	}
	// Still answered with CORS, so the browser can read the refusal.
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
}

func TestATokenThatProvesNobodyIsRefused(t *testing.T) {
	r := newPeopleRig(t)
	for _, bearer := range []string{"", "nonsense", "broker"} {
		rr := r.personToken(bearer, "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("bearer %q: code = %d, want 401: %s", bearer, rr.Code, rr.Body.String())
			continue
		}
		var body struct{ Error string }
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		if body.Error != throwaway.ErrorCode {
			t.Errorf("bearer %q: error = %q, want %s", bearer, body.Error, throwaway.ErrorCode)
		}
	}
	if len(r.gitea.Tokens(roles.Login("u-jan"))) != 0 {
		t.Errorf("a refused call minted a token")
	}
}

func TestAPersonWhoIsNotAnActiveMemberGetsNothing(t *testing.T) {
	r := newPeopleRig(t)
	// Gone's throwaway passes the shape checks but its creator is only
	// invited: the checker refuses at its last step.
	rr := r.personToken("throwaway-gone", "")
	if rr.Code == http.StatusOK {
		t.Fatalf("an invited person got a token: %s", rr.Body.String())
	}
	if _, ok := r.gitea.User(roles.Login("u-gone")); ok {
		t.Errorf("an account was made for an invited person")
	}
	if got := r.passes.Load(); got != 0 {
		t.Errorf("passes = %d, want 0", got)
	}
}

func TestPersonTokenAnswersEveryOrigin(t *testing.T) {
	r := newPeopleRig(t)
	// D22: the proof is the bearer, never the origin. A Gitea made from
	// mate.zerops.io is driven from a developer's localhost, and back.
	for _, origin := range []string{
		"https://mate.zerops.io", "http://localhost:5734", "http://127.0.0.1:5734", "https://elsewhere.example",
	} {
		req := httptest.NewRequest(http.MethodOptions, "/person/token", nil)
		req.Header.Set("Origin", origin)
		rr := r.do(req)
		if rr.Code != http.StatusNoContent {
			t.Errorf("OPTIONS from %s = %d", origin, rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("origin %s: Access-Control-Allow-Origin = %q, want *", origin, got)
		}
		if got := rr.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
			t.Errorf("origin %s: methods = %q, want POST", origin, got)
		}
		if got := rr.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
			t.Errorf("origin %s: headers = %q, want Authorization", origin, got)
		}
	}
}

func TestPersonTokenIsNotServedWithoutAProver(t *testing.T) {
	r := newRig(t)
	rr := r.do(httptest.NewRequest(http.MethodPost, "/person/token", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("POST /person/token with no prover = %d, want 404", rr.Code)
	}
	var out gitea.User
	_ = out
}

// The run of 2026-09-19: a fresh account's Gitea published a site-admin pair
// it then refused, so every call the broker made as site admin came back 401.
// The app showed the person "Gitea refused: invalid username, password or
// token" — about credentials that were never theirs, on a sign-in that was
// perfectly good. A 401 or 403 on this path is the broker's own credential, and
// is answered as that.
func TestGiteaRefusingTheBrokersOwnCredentialIsNotThePersonsRefusal(t *testing.T) {
	r := newPeopleRig(t)
	r.server.deps.Gitea = r.gitea.TokenOnly("not-the-site-admins-token")

	rr := r.personToken("throwaway-jan", "https://app.example")
	if rr.Code != http.StatusFailedDependency {
		t.Fatalf("POST /person/token = %d, want 424: %s", rr.Code, rr.Body.String())
	}
	var body struct{ Error, Message string }
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Error != "gitea_admin_refused" {
		t.Errorf("error = %q, want gitea_admin_refused", body.Error)
	}
	if strings.Contains(body.Message, "invalid username, password or token") {
		t.Errorf("message relays Gitea's words at the person: %q", body.Message)
	}
	if !strings.Contains(body.Message, "administrator credentials") {
		t.Errorf("message = %q, want it to name whose credentials failed", body.Message)
	}
}

// memberListReads counts the calls that read the org's member list.
func (p *peopleRig) memberListReads() int {
	n := 0
	for _, request := range p.zerops.Requests {
		if request == "GET /client/"+clientID+"/user/list" {
			n++
		}
	}
	return n
}

// TestTheMemberListIsReadOncePerPersonTokenRequest — the throwaway's last step
// reads the member list, and the rights that follow are computed from that
// same read: every extra read of it was one more chance for Zerops to answer
// 400 (the intermittent 502s of 2026-09-2x).
func TestTheMemberListIsReadOncePerPersonTokenRequest(t *testing.T) {
	r := newPeopleRig(t)
	rr := r.personToken("throwaway-jan", "https://app.example")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /person/token = %d %s", rr.Code, rr.Body.String())
	}
	if got := r.memberListReads(); got != 1 {
		t.Fatalf("the member list was read %d times, want once: %v", got, r.zerops.Requests)
	}
}

// TestAZeropsRefusalOnTheMemberListIsRetried — Zerops answers the member list
// 400 now and then, for no reason the broker can act on; a short ladder of
// retries turns that into a sign-in that works.
func TestAZeropsRefusalOnTheMemberListIsRetried(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			r := newPeopleRig(t)
			members := "GET /client/" + clientID + "/user/list"
			r.zerops.Fail[members] = status
			r.zerops.FailTimes[members] = 2

			rr := r.personToken("throwaway-jan", "https://app.example")
			if rr.Code != http.StatusOK {
				t.Fatalf("POST /person/token = %d %s, want the refusals retried", rr.Code, rr.Body.String())
			}
			if got := r.memberListReads(); got != 3 {
				t.Fatalf("the member list was read %d times, want 2 refusals and 1 answer", got)
			}
		})
	}
}

// TestPersonTokenNeverAnswers502AndEveryErrorCarriesCORS — the platform's edge
// replaces an upstream 502 with its own page and no CORS headers (measured
// 2026-09-17), so the browser saw a network error instead of the broker's
// answer. Zerops or Gitea being away is a 503 with Retry-After, carrying the
// same CORS headers as a success.
func TestPersonTokenNeverAnswers502AndEveryErrorCarriesCORS(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(r *peopleRig)
	}{
		{"Zerops refuses the member list every time", func(r *peopleRig) {
			r.zerops.Fail["GET /client/"+clientID+"/user/list"] = http.StatusBadRequest
		}},
		{"Zerops fails the member list every time", func(r *peopleRig) {
			r.zerops.Fail["GET /client/"+clientID+"/user/list"] = http.StatusInternalServerError
		}},
		{"Zerops fails the project list the rights need", func(r *peopleRig) {
			r.zerops.Fail["POST /project/search"] = http.StatusInternalServerError
		}},
		{"Gitea fails", func(r *peopleRig) {
			r.gitea.Fail["GET /users/"+roles.Login("u-jan")] = http.StatusInternalServerError
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPeopleRig(t)
			tc.breakIt(r)

			rr := r.personToken("throwaway-jan", "https://app.example")
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("POST /person/token = %d %s, want 503", rr.Code, rr.Body.String())
			}
			if rr.Header().Get("Retry-After") == "" {
				t.Errorf("a 503 carries no Retry-After")
			}
			if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
			}
			if got := rr.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
				t.Errorf("Access-Control-Allow-Headers = %q, want Authorization", got)
			}
		})
	}
}
