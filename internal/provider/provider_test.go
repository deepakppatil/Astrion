package provider

import (
	"errors"
	"testing"

	"github.com/deepakppatil/astrion/internal/domain"
)

func pools() []*Pool {
	return []*Pool{
		{Name: "rancher-private", Type: "kubernetes", Network: "PRIVATE", MaxClassification: domain.ClassRestricted, CostWeight: 5, Enabled: true},
		{Name: "aws-eks-public", Type: "kubernetes", Network: "PUBLIC", MaxClassification: domain.ClassInternal, CostWeight: 3, Enabled: true, Region: "eu-west-1"},
		{Name: "disabled", Type: "kubernetes", Network: "PUBLIC", MaxClassification: domain.ClassRestricted, Enabled: false},
	}
}

func TestSelectPrefersPrivateCompliantPool(t *testing.T) {
	p, err := Select(pools(), SelectionRequest{Requested: "AUTO", DataClassification: domain.ClassInternal})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "rancher-private" {
		t.Fatalf("got %s", p.Name)
	}
}

func TestSelectNeverMovesRestrictedDataToPublic(t *testing.T) {
	ps := pools()
	ps[0].Enabled = false // private capacity "gone"
	_, err := Select(ps, SelectionRequest{Requested: "AUTO", DataClassification: domain.ClassConfidential})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("expected safe failure, got %v", err)
	}
	// Explicitly requesting the public pool must also be refused.
	_, err = Select(pools(), SelectionRequest{Requested: "aws-eks-public", DataClassification: domain.ClassRestricted})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("expected refusal, got %v", err)
	}
}

func TestSelectHonorsTenantAndWorkspacePermissions(t *testing.T) {
	p, err := Select(pools(), SelectionRequest{Requested: "AUTO", DataClassification: domain.ClassPublic, TenantPermitted: []string{"aws-eks-public"}})
	if err != nil || p.Name != "aws-eks-public" {
		t.Fatalf("got %v %v", p, err)
	}
	_, err = Select(pools(), SelectionRequest{Requested: "AUTO", DataClassification: domain.ClassPublic, TenantPermitted: []string{"aws-eks-public"}, WorkspacePermitted: []string{"rancher-private"}})
	if err == nil {
		t.Fatal("expected no pool")
	}
}

func TestSelectPrivateNetworkRepo(t *testing.T) {
	ps := pools()[1:] // only public
	_, err := Select(ps, SelectionRequest{Requested: "AUTO", DataClassification: domain.ClassPublic, PrivateNetworkRepo: true})
	if err == nil {
		t.Fatal("private repo must not run on public pool")
	}
}

func TestUnknownClassificationTreatedAsMostSensitive(t *testing.T) {
	_, err := Select(pools()[1:2], SelectionRequest{Requested: "AUTO", DataClassification: "WHATEVER"})
	if err == nil {
		t.Fatal("unknown classification must fail safe")
	}
}
