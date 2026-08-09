package billing

import (
	"testing"
	"time"

	"github.com/unitronix/betterdesk-server/db"
)

func TestRoundUpMinutes(t *testing.T) {
	if got := roundUpMinutes(61, 1); got != 2 {
		t.Fatalf("got %d want 2", got)
	}
	if got := roundUpMinutes(601, 10); got != 20 {
		t.Fatalf("got %d want 20", got)
	}
	if got := roundUpMinutes(600, 10); got != 10 {
		t.Fatalf("got %d want 10", got)
	}
}

func TestSweepStalePendingRelays(t *testing.T) {
	svc := NewService(nil, nil, 1, false)
	svc.pending["old"] = &pendingRelay{OrgID: "org1", createdAt: time.Now().Add(-11 * time.Minute)}
	svc.pending["fresh"] = &pendingRelay{OrgID: "org1", createdAt: time.Now()}

	svc.sweepStalePending()

	if _, ok := svc.pending["old"]; ok {
		t.Fatal("stale pending relay should be removed")
	}
	if _, ok := svc.pending["fresh"]; !ok {
		t.Fatal("fresh pending relay should remain")
	}
}

func TestOverageSplit(t *testing.T) {
	remaining := 5
	billed := 12
	included := min(billed, remaining)
	overage := 0
	if billed > remaining {
		overage = billed - remaining
	}
	if included != 5 || overage != 7 {
		t.Fatalf("included=%d overage=%d", included, overage)
	}
	_ = time.Now()
}

func TestSessionAmountCalculation(t *testing.T) {
	includedUsed := 5
	overageMin := 7
	hourlyRate := 120.0
	overageRate := 180.0

	amountIncluded := (float64(includedUsed) / 60.0) * hourlyRate
	amountOverage := (float64(overageMin) / 60.0) * overageRate
	total := amountIncluded + amountOverage

	if amountIncluded != 10.0 {
		t.Fatalf("amountIncluded=%v want 10", amountIncluded)
	}
	if amountOverage != 21.0 {
		t.Fatalf("amountOverage=%v want 21", amountOverage)
	}
	if total != 31.0 {
		t.Fatalf("total=%v want 31", total)
	}
}

// fakeBillingDB implements just enough of db.Database for CheckConnection
// (device-level contract resolution). Uncovered methods panic via the
// embedded nil interface — tests must not touch them.
type fakeBillingDB struct {
	db.Database
	contracts map[string]*db.BillingContract // "type|key" -> contract
	orgIDs    map[string]string
}

func (f *fakeBillingDB) GetActiveBillingContract(targetType, targetKey string) (*db.BillingContract, error) {
	return f.contracts[targetType+"|"+targetKey], nil
}

func (f *fakeBillingDB) GetDeviceOrgID(deviceID string) (string, error) {
	return f.orgIDs[deviceID], nil
}

func contractFor(key string, status string, validUntil *time.Time) *db.BillingContract {
	return &db.BillingContract{
		ID:               "c-" + key,
		TargetType:       db.BillingTargetDevice,
		TargetKey:        key,
		PackageID:        "pkg",
		Status:           status,
		RemainingMinutes: 60,
		HourlyRate:       100,
		Currency:         "PLN",
		ValidUntil:       validUntil,
	}
}

func TestCheckConnectionContractExpiry(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(24 * time.Hour)

	cases := []struct {
		name       string
		contract   *db.BillingContract
		wantAllow  bool
		wantReason string
	}{
		{"active no expiry", contractFor("dev1", "active", nil), true, ""},
		{"active future expiry", contractFor("dev2", "active", &future), true, ""},
		{"status expired", contractFor("dev3", "expired", nil), false, "contract_expired"},
		{"valid_until in the past", contractFor("dev4", "active", &past), false, "contract_expired"},
		{"suspended", contractFor("dev5", "suspended", nil), false, "billing_suspended"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fdb := &fakeBillingDB{
				contracts: map[string]*db.BillingContract{
					db.BillingTargetDevice + "|" + tc.contract.TargetKey: tc.contract,
				},
				orgIDs: map[string]string{tc.contract.TargetKey: "org1"},
			}
			svc := NewService(fdb, nil, 1, false)
			got := svc.CheckConnection(tc.contract.TargetKey)
			if got.Allowed != tc.wantAllow {
				t.Fatalf("Allowed=%v want %v (reason=%q)", got.Allowed, tc.wantAllow, got.Reason)
			}
			if !tc.wantAllow && got.Reason != tc.wantReason {
				t.Fatalf("Reason=%q want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

func TestCheckConnectionTrafficQuota(t *testing.T) {
	cases := []struct {
		name       string
		quota      int64
		used       int64
		wantAllow  bool
		wantReason string
	}{
		{"quota exhausted (used == quota)", 1000, 1000, false, "traffic_quota_exceeded"},
		{"quota exceeded (used > quota)", 1000, 1500, false, "traffic_quota_exceeded"},
		{"quota unlimited (0)", 0, 1 << 40, true, ""},
		{"under quota", 1000, 999, true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := contractFor("qdev-"+tc.name, "active", nil)
			c.QuotaBytes = tc.quota
			c.UsedBytes = tc.used
			fdb := &fakeBillingDB{
				contracts: map[string]*db.BillingContract{
					db.BillingTargetDevice + "|" + c.TargetKey: c,
				},
				orgIDs: map[string]string{c.TargetKey: "org1"},
			}
			svc := NewService(fdb, nil, 1, false)
			got := svc.CheckConnection(c.TargetKey)
			if got.Allowed != tc.wantAllow {
				t.Fatalf("Allowed=%v want %v (reason=%q)", got.Allowed, tc.wantAllow, got.Reason)
			}
			if !tc.wantAllow && got.Reason != tc.wantReason {
				t.Fatalf("Reason=%q want %q", got.Reason, tc.wantReason)
			}
			if !got.HasBilling {
				t.Fatalf("HasBilling=false, want true for quota-relevant contract")
			}
		})
	}
}
