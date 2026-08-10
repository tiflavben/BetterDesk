package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/unitronix/betterdesk-server/config"
	"github.com/unitronix/betterdesk-server/db"
	"github.com/unitronix/betterdesk-server/peer"
)

// startTestBillingServer boots a full API server on a dedicated port.
func startTestBillingServer(t *testing.T, database db.Database, port int) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.APIPort = port
	peerMap := peer.NewMap()
	srv := New(cfg, database, peerMap, nil, "1.0.0-test")
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	time.Sleep(100 * time.Millisecond)
}

// postBillingContract sends an authenticated POST to /api/billing/contracts
// and returns the decoded response.
func postBillingContract(t *testing.T, port int, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/billing/contracts", port), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(testAuthReq(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, out
}

// TestCreateBillingContractAutoCreatesDefaultPackage verifies that a user
// contract can be created with NO package_id when the packages table is
// empty: the server must auto-create a default package and link the contract
// to it (previously this returned 400 "package_id required").
func TestCreateBillingContractAutoCreatesDefaultPackage(t *testing.T) {
	database := testSetupDB(t)
	defer database.Close()

	pkgs, err := database.ListBillingPackages()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 0 {
		t.Fatalf("precondition: expected empty packages table, got %d", len(pkgs))
	}

	startTestBillingServer(t, database, 19976)

	status, out := postBillingContract(t, 19976, `{"target_type":"user","target_key":"auto-pkg-user-1","status":"active","device_limit":5,"quota_bytes":104857600}`)
	if status != http.StatusCreated {
		t.Fatalf("POST contract status=%d want 201, body=%v", status, out)
	}
	contractPkgID, _ := out["package_id"].(string)
	if contractPkgID == "" {
		t.Fatalf("contract.package_id empty, server did not auto-resolve a package: %v", out)
	}

	// A default package must now exist and be the one referenced.
	pkgs, err = database.ListBillingPackages()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("expected exactly 1 auto-created package, got %d", len(pkgs))
	}
	if pkgs[0].Name != "default" {
		t.Fatalf("auto-created package name=%q want \"default\"", pkgs[0].Name)
	}
	if pkgs[0].ID != contractPkgID {
		t.Fatalf("contract package_id=%q does not match auto-created package %q", contractPkgID, pkgs[0].ID)
	}
	if pkgs[0].Currency != "PLN" {
		t.Fatalf("auto-created package currency=%q want PLN", pkgs[0].Currency)
	}
}

// TestCreateBillingContractAutoUsesFirstPackage verifies that when packages
// already exist, an empty package_id resolves to the first package.
func TestCreateBillingContractAutoUsesFirstPackage(t *testing.T) {
	database := testSetupDB(t)
	defer database.Close()

	first := &db.BillingPackage{ID: "pkg-first", Name: "First Pkg", Description: "d", IncludedMinutes: 30, OverageRate: 1.5, Currency: "PLN"}
	if err := database.CreateBillingPackage(first); err != nil {
		t.Fatal(err)
	}

	startTestBillingServer(t, database, 19977)

	status, out := postBillingContract(t, 19977, `{"target_type":"user","target_key":"auto-pkg-user-2","status":"active"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST contract status=%d want 201, body=%v", status, out)
	}
	if got, _ := out["package_id"].(string); got != first.ID {
		t.Fatalf("contract package_id=%q want %q (first existing package)", got, first.ID)
	}

	// No second package may have been created.
	pkgs, err := database.ListBillingPackages()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("expected package table unchanged (1), got %d", len(pkgs))
	}
}

// TestCreateBillingContractStillRejectsBadTarget verifies the existing
// contract validation is untouched by the package_id change.
func TestCreateBillingContractStillRejectsBadTarget(t *testing.T) {
	database := testSetupDB(t)
	defer database.Close()

	startTestBillingServer(t, database, 19978)

	status, out := postBillingContract(t, 19978, `{"target_type":"nope","target_key":"x","status":"active"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST contract with invalid target_type status=%d want 400, body=%v", status, out)
	}
}
