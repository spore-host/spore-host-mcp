package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spore-host/lagotto/pkg/testutil/hermetic"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

// TestHermeticGuard_CatchesAnUnfakedClient pins the guard itself (#79): a
// client built straight from spawnclient.NewClient, as lagottoContext did
// before #79, signs with the fake credentials and lands on the recorder, not
// on AWS. The test clears the request it made on purpose so the binary does
// not fail on it.
func TestHermeticGuard_CatchesAnUnfakedClient(t *testing.T) {
	if hermeticGuard == nil {
		t.Fatal("TestMain did not install the hermetic guard")
	}
	if got := os.Getenv("AWS_ACCESS_KEY_ID"); got != hermetic.FakeAccessKeyID {
		t.Fatalf("AWS_ACCESS_KEY_ID = %q, want the hermetic fake", got)
	}
	if os.Getenv("AWS_PROFILE") != "" {
		t.Fatal("AWS_PROFILE survived the hermetic environment")
	}

	client, err := spawnclient.NewClient(context.Background())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = sts.NewFromConfig(client.Config()).GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
	if err == nil {
		t.Fatal("GetCallerIdentity through an unfaked client succeeded; it must be refused")
	}
	seen := hermeticGuard.Take()
	if len(seen) == 0 {
		t.Fatalf("the recorder saw nothing; the request went elsewhere (err: %v)", err)
	}
	if !strings.Contains(strings.Join(seen, "\n"), "GetCallerIdentity") {
		t.Errorf("recorder saw %v, want the GetCallerIdentity call", seen)
	}
}

// TestLagottoContext_UsesTheSpawnClientSeam is #79's fix: the lagotto tools
// build their AWS config through spawnClient, so an installed fakeAWS answers
// them. Before #79 lagottoContext called spawnclient.NewClient itself and this
// would have reached the hermetic recorder (or, before the guard, real AWS).
func TestLagottoContext_UsesTheSpawnClientSeam(t *testing.T) {
	f := newFakeAWS(t)
	f.account = "210987654321"
	f.install(t)

	store, cfg, caller, err := lagottoContext(context.Background())
	if err != nil {
		t.Fatalf("lagottoContext: %v", err)
	}
	if store == nil {
		t.Fatal("nil store")
	}
	if want := "arn:aws:iam::210987654321:user/test"; caller != want {
		t.Errorf("caller = %q, want %q (the fake's identity)", caller, want)
	}
	if cfg.Region != "us-west-2" {
		t.Errorf("region = %q, want the fake client's us-west-2", cfg.Region)
	}
}
