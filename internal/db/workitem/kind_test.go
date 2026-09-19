package workitem

import (
	"strings"
	"testing"
	"time"
)

// TestKindValidate pins the refusals. Every one of them is a case where the
// alternative would be a silently wrong write: the wrong dialect's syntax, an
// interpolated column name, a unit deadline that cannot finish inside its own
// lease.
func TestKindValidate(t *testing.T) {
	base := Kind{Table: "fixture", Dialect: Postgres, Strategy: SingleTx, Columns: []string{"payload"}}
	if err := base.Validate(); err != nil {
		t.Fatalf("a minimal kind should validate: %v", err)
	}

	for _, tc := range []struct {
		name string
		want string
		kind Kind
	}{
		{"no table", "no table", Kind{Dialect: Postgres, Strategy: SingleTx}},
		{"table is not an identifier", "identifier", with(base, func(k *Kind) { k.Table = "public.fixture" })},
		{"table too long for an index name", "identifier", with(base, func(k *Kind) { k.Table = strings.Repeat("x", 46) })},
		{"no dialect", "dialect unset", with(base, func(k *Kind) { k.Dialect = DialectUnset })},
		{"no strategy", "execution strategy", with(base, func(k *Kind) { k.Strategy = StrategyUnset })},
		{"unknown unique mode", "unique mode", with(base, func(k *Kind) { k.Unique = UniqueMode(9) })},
		{"column is not an identifier", "identifier", with(base, func(k *Kind) { k.Columns = []string{"pay load"} })},
		{"column shadows the block", "block column", with(base, func(k *Kind) { k.Columns = []string{"status"} })},
		{"duplicate column", "twice", with(base, func(k *Kind) { k.Columns = []string{"payload", "payload"} })},
		{"frozen column undeclared", "not one of its declared columns", with(base, func(k *Kind) { k.Frozen = []string{"other"} })},
		{"negative attempt budget", "MaxAttempts", with(base, func(k *Kind) { k.Policy.MaxAttempts = -1 })},
		{"unit deadline reaches the lease", "UnitDeadline", with(base, func(k *Kind) {
			k.Policy.Lease, k.Policy.UnitDeadline = time.Minute, time.Minute
		})},
		{"renewal interval reaches the lease", "RenewEvery", with(base, func(k *Kind) {
			k.Policy.Lease, k.Policy.RenewEvery = time.Minute, 2*time.Minute
		})},
		{"jitter out of range", "jitter", with(base, func(k *Kind) { k.Policy.Backoff.Jitter = 1 })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.kind.Validate()
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestPolicyDefaults pins that a zero Policy is legal and resolves to the
// documented defaults, so a kind opting into nothing still gets a coherent
// lease, renewal interval and budget.
func TestPolicyDefaults(t *testing.T) {
	p := Policy{}.resolved()
	if p.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", p.MaxAttempts, DefaultMaxAttempts)
	}
	if p.Lease != DefaultLease {
		t.Errorf("Lease = %s, want %s", p.Lease, DefaultLease)
	}
	if p.RenewEvery != DefaultLease/3 {
		t.Errorf("RenewEvery = %s, want a third of the lease", p.RenewEvery)
	}
	if p.UnitDeadline != DefaultLease/2 {
		t.Errorf("UnitDeadline = %s, want half the lease", p.UnitDeadline)
	}
	if p.Backoff != (BackoffSpec{DefaultBackoffBase, DefaultBackoffCap, DefaultBackoffJitter}) {
		t.Errorf("Backoff = %+v, want the defaults", p.Backoff)
	}

	// The derived defaults follow a shortened lease rather than the constant,
	// which is what keeps a 300ms-lease kind valid.
	short := Policy{Lease: 300 * time.Millisecond}.resolved()
	if short.RenewEvery >= short.Lease || short.UnitDeadline >= short.Lease {
		t.Errorf("derived defaults %+v do not fit inside a %s lease", short, short.Lease)
	}
}

// TestIndexDDLByMode pins the one thing that varies with the uniqueness mode,
// since an adopting table's migration copies this text verbatim.
func TestIndexDDLByMode(t *testing.T) {
	k := Kind{Table: "fixture", Dialect: Postgres, Strategy: SingleTx}
	if got := len(IndexDDL(with(k, func(k *Kind) { k.Unique = UniqueNone }))); got != 3 {
		t.Errorf("UniqueNone renders %d statements, want the three claim indexes", got)
	}

	forever := IndexDDL(with(k, func(k *Kind) { k.Unique = UniqueForever }))
	if len(forever) != 4 || strings.Contains(forever[3], "status IN") {
		t.Errorf("UniqueForever uniqueness index = %q, want no status predicate", forever[3])
	}
	unsettled := IndexDDL(with(k, func(k *Kind) { k.Unique = UniqueWhileUnsettled }))
	if len(unsettled) != 4 || !strings.Contains(unsettled[3], "status IN ('ready','leased','parked')") {
		t.Errorf("UniqueWhileUnsettled uniqueness index = %q, want the unsettled predicate", unsettled[3])
	}
	for i, name := range IndexNames(with(k, func(k *Kind) { k.Unique = UniqueWhileUnsettled })) {
		if !strings.Contains(unsettled[i], " "+name+" ") {
			t.Errorf("IndexNames[%d] = %q is not the name IndexDDL[%d] creates: %q", i, name, i, unsettled[i])
		}
	}
}

func with(k Kind, mutate func(*Kind)) Kind {
	mutate(&k)
	return k
}
