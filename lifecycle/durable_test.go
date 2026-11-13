//go:build !minimal

package lifecycle

import (
	"context"
	"github.com/MalenkiySolovey/solovey-host/componenthost/durable"
	"github.com/MalenkiySolovey/solovey-host/hostref"
	"testing"
	"time"
)

type testPort struct {
	scope durable.Scope
	calls int
}

func (p *testPort) Scope() durable.Scope { return p.scope }
func (p *testPort) MigrateAndValidate(context.Context) (durable.Result, error) {
	p.calls++
	return durable.Result{Scope: p.scope, Outcome: durable.Verified}, nil
}
func (p *testPort) NormalizeRestore(context.Context) (durable.Result, error) {
	p.calls++
	return durable.Result{Scope: p.scope, Outcome: durable.Verified}, nil
}
func (p *testPort) DropOwnedData(context.Context) (durable.Result, error) {
	p.calls++
	return durable.Result{Scope: p.scope, Outcome: durable.Verified}, nil
}
func (p *testPort) InspectDropAuthority(context.Context, time.Time) (durable.AuthorityStatus, error) {
	p.calls++
	return durable.AuthorityStatus{State: "EMPTY"}, nil
}
func TestDurableHooksRejectForeignAndWrongStorageScopesBeforeEffects(t *testing.T) {
	owner := Owner{}
	valid := durable.Scope{Host: hostref.Local(), OwnerID: "fallback-html", Storage: durable.Live, SchemaVersion: "1", MigrationVersion: "1"}
	for _, tc := range []struct {
		name  string
		scope durable.Scope
	}{{"foreign owner", func() durable.Scope { s := valid; s.OwnerID = "other"; return s }()}, {"unknown schema", func() durable.Scope { s := valid; s.SchemaVersion = "2"; return s }()}, {"unknown migration", func() durable.Scope { s := valid; s.MigrationVersion = "2"; return s }()}, {"missing storage", func() durable.Scope { s := valid; s.Storage = ""; return s }()}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &testPort{scope: tc.scope}
			if owner.Migrate(context.Background(), p) == nil || owner.RehearseRestore(context.Background(), p) == nil || owner.DropData(context.Background(), p) == nil {
				t.Fatal("foreign scope accepted")
			}
			if _, err := owner.InspectDropAuthority(context.Background(), p, time.Now()); err == nil {
				t.Fatal("foreign inspection accepted")
			}
			if p.calls != 0 {
				t.Fatal("foreign operation executed")
			}
		})
	}
	live := &testPort{scope: valid}
	if err := owner.RehearseRestore(context.Background(), live); err == nil || live.calls != 0 {
		t.Fatal("live restore accepted")
	}
	candidate := &testPort{scope: valid}
	candidate.scope.Storage = durable.Candidate
	if err := owner.DropData(context.Background(), candidate); err == nil || candidate.calls != 0 {
		t.Fatal("candidate data drop accepted")
	}
	if err := owner.Migrate(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if err := owner.RehearseRestore(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if err := owner.DropData(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if live.calls != 2 || candidate.calls != 1 {
		t.Fatal("admitted operation count differs")
	}
}
