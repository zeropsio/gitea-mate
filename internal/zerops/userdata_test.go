package zerops_test

import (
	"context"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// A service's own variables: read as a list, created one at a time, updated
// by id with the key beside the content — the three calls the rights loop
// delivers a Mate's Gitea access with (measured 2026-09-16 and 2026-09-17).
func TestServiceUserDataReadCreateUpdate(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	f.SetServices("p-fen", zerops.Service{
		ID: "s-zcp", ProjectID: "p-fen", Name: "zcp", Status: "ACTIVE",
		TypeInfo: zerops.ServiceTypeInfo{VersionName: "zcp@1"},
	})
	f.SetUserData("s-zcp", zerops.ServiceUserData{ID: "ud-1", Key: "ZCP_API_KEY", Content: "k", Sensitive: true})

	// The type version travels on the search: it is how a Mate's container is
	// found.
	services, err := c.Services(ctx, org, "p-fen")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(services) != 1 || services[0].TypeInfo.VersionName != "zcp@1" {
		t.Fatalf("services = %+v", services)
	}

	// A sensitive value comes back in clear to a token on the project.
	have, err := c.UserData(ctx, "s-zcp")
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	if len(have) != 1 || have[0].ID != "ud-1" || have[0].Content != "k" || !have[0].Sensitive {
		t.Fatalf("user data = %+v", have)
	}

	if _, err := c.CreateUserData(ctx, "s-zcp", zerops.UserDataSpec{Key: "GITEA_TOKEN", Content: "t1", Sensitive: true}); err != nil {
		t.Fatalf("CreateUserData: %v", err)
	}
	have, err = c.UserData(ctx, "s-zcp")
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	var token zerops.ServiceUserData
	for _, entry := range have {
		if entry.Key == "GITEA_TOKEN" {
			token = entry
		}
	}
	if token.ID == "" || token.Content != "t1" || !token.Sensitive {
		t.Fatalf("the created variable = %+v", token)
	}

	if err := c.UpdateUserData(ctx, token.ID, "GITEA_TOKEN", "t2"); err != nil {
		t.Fatalf("UpdateUserData: %v", err)
	}
	for _, entry := range f.UserData("s-zcp") {
		if entry.Key == "GITEA_TOKEN" && (entry.Content != "t2" || entry.ID != token.ID || !entry.Sensitive) {
			t.Errorf("after the update = %+v", entry)
		}
	}
}

// A project the broker's token was never granted answers 403 on its service
// search; the caller reads the status and treats it as "not yet".
func TestAnUngrantedProjectIs403(t *testing.T) {
	f, c := newFake(t)
	f.SetServices("p-fen", zerops.Service{ID: "s-zcp", ProjectID: "p-fen", Name: "zcp"})
	f.Ungranted["p-fen"] = true

	_, err := c.Services(context.Background(), org, "p-fen")
	if zerops.Status(err) != 403 {
		t.Fatalf("err = %v, want a 403", err)
	}
	_, err = c.UserData(context.Background(), "s-zcp")
	if zerops.Status(err) != 403 {
		t.Fatalf("err = %v, want a 403", err)
	}
}

// PUT /user-data/{id} must carry the key beside the content: the fake refuses
// a body without one the way the platform does, and the client always sends it.
func TestUpdateUserDataCarriesTheKey(t *testing.T) {
	f, c := newFake(t)
	f.SetServices("p-fen", zerops.Service{ID: "s-zcp", ProjectID: "p-fen", Name: "zcp"})
	f.SetUserData("s-zcp", zerops.ServiceUserData{ID: "ud-1", Key: "GITEA_URL", Content: "https://old"})

	if err := c.UpdateUserData(context.Background(), "ud-1", "GITEA_URL", "https://new"); err != nil {
		t.Fatalf("UpdateUserData: %v", err)
	}
	if got := f.UserData("s-zcp"); len(got) != 1 || got[0].Content != "https://new" {
		t.Errorf("after the update = %+v", got)
	}
	if err := c.UpdateUserData(context.Background(), "ud-9", "GITEA_URL", "x"); zerops.Status(err) != 404 {
		t.Errorf("an unknown id = %v, want a 404", err)
	}
}
