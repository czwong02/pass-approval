package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"pass-approval/internal/channel"
	"pass-approval/internal/db"
	"pass-approval/internal/rules"
	"pass-approval/internal/token"
)

func TestChecks(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	t.Run("all", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		created := mustCreate(t, ctx, conn, fx, input("all", []string{"ada", "bob"}, nil))
		if len(fx.mem.Sent) != 2 {
			t.Fatalf("sent %d", len(fx.mem.Sent))
		}
		tok := approve(t, fx.mem, "ext_ada")
		mustDecide(t, ctx, conn, fx, tok, "ext_ada")
		if status(t, ctx, conn, fx, created.ID) != "pending" || decisions(t, ctx, conn, fx, created.ID) != 1 {
			t.Fatal("after ada")
		}
		mustDecide(t, ctx, conn, fx, tok, "ext_ada")
		if decisions(t, ctx, conn, fx, created.ID) != 1 {
			t.Fatal("replay")
		}
		mustDecide(t, ctx, conn, fx, approve(t, fx.mem, "ext_bob"), "ext_bob")
		if status(t, ctx, conn, fx, created.ID) != "approved" || decisions(t, ctx, conn, fx, created.ID) != 2 {
			t.Fatal("after bob")
		}
	})

	t.Run("sequence", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		created := mustCreate(t, ctx, conn, fx, input("sequence", nil, []rules.Step{
			{Rule: "any", Approvers: []string{"ada"}},
			{Rule: "any", Approvers: []string{"bob"}},
		}))
		if len(fx.mem.Sent) != 1 || fx.mem.Sent[0].To != "ext_ada" {
			t.Fatal("step 1 only")
		}
		mustDecide(t, ctx, conn, fx, approve(t, fx.mem, "ext_ada"), "ext_ada")
		if status(t, ctx, conn, fx, created.ID) != "pending" || len(fx.mem.Sent) != 2 {
			t.Fatal("step 2")
		}
	})

	t.Run("reject", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		created := mustCreate(t, ctx, conn, fx, input("any", []string{"ada", "bob"}, nil))
		mustDecide(t, ctx, conn, fx, reject(t, fx.mem, "ext_ada"), "ext_ada")
		if status(t, ctx, conn, fx, created.ID) != "rejected" || decisions(t, ctx, conn, fx, created.ID) != 1 {
			t.Fatal("veto")
		}
	})

	t.Run("repeat", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		in := input("any", []string{"ada"}, nil)
		first := mustCreate(t, ctx, conn, fx, in)
		n := len(fx.mem.Sent)
		second := mustCreate(t, ctx, conn, fx, in)
		if second.ID != first.ID || len(fx.mem.Sent) != n {
			t.Fatal("repeat sent again")
		}
	})

	t.Run("unlinked", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		created := mustCreate(t, ctx, conn, fx, input("any", []string{"ada"}, nil))
		if _, err := conn.Exec(ctx, `delete from channel_identities where tenant_id = $1`, fx.tenant); err != nil {
			t.Fatal(err)
		}
		mustDecide(t, ctx, conn, fx, approve(t, fx.mem, "ext_ada"), "ext_ada")
		if status(t, ctx, conn, fx, created.ID) != "pending" || decisions(t, ctx, conn, fx, created.ID) != 0 {
			t.Fatal("unlinked wrote a decision")
		}
	})
}

type fx struct {
	tenant  string
	mem     *channel.Memory
	ch      Channels
	cleanup func()
}

func fixture(t *testing.T, ctx context.Context, conn *pgx.Conn) *fx {
	t.Helper()
	id := token.NewID("ten_")
	if _, err := conn.Exec(ctx, `insert into tenants (id, name, api_key_hash, callback_secret) values ($1,'check',$1,'secret')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `insert into users (id, tenant_id, name) values ('ada',$1,'Ada'), ('bob',$1,'Bob')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		insert into channel_identities (tenant_id, user_id, channel, external_user_id)
		values ($1,'ada','test','ext_ada'), ($1,'bob','test','ext_bob')`, id); err != nil {
		t.Fatal(err)
	}
	mem := &channel.Memory{}
	return &fx{
		tenant: id, mem: mem, ch: Channels{"test": mem},
		cleanup: func() { _, _ = conn.Exec(ctx, `delete from tenants where id = $1`, id) },
	}
}

func input(rule string, approvers []string, steps []rules.Step) CreateInput {
	return CreateInput{
		ExternalRef: token.NewID("ref_"), Title: "Deploy", Body: "sha",
		Fields: []channel.Field{{Label: "Env", Value: "prod"}},
		Rule:   rule, Approvers: approvers, Steps: steps,
		CallbackURL: "https://example.com/hook", ExpiresIn: 3600,
	}
}

func mustCreate(t *testing.T, ctx context.Context, conn *pgx.Conn, fx *fx, in CreateInput) Created {
	t.Helper()
	created, err := Create(ctx, conn, fx.tenant, in, fx.ch)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func mustDecide(t *testing.T, ctx context.Context, conn *pgx.Conn, fx *fx, raw, ext string) {
	t.Helper()
	err := Decide(ctx, conn, struct {
		Token, ExternalUserID, Channel, Action string
	}{raw, ext, "test", "approve"}, fx.ch)
	if err != nil {
		t.Fatal(err)
	}
}

func approve(t *testing.T, m *channel.Memory, to string) string {
	t.Helper()
	return action(t, m, to, "Approve")
}

func reject(t *testing.T, m *channel.Memory, to string) string {
	t.Helper()
	return action(t, m, to, "Reject")
}

func action(t *testing.T, m *channel.Memory, to, label string) string {
	t.Helper()
	for _, s := range m.Sent {
		if s.To != to {
			continue
		}
		for _, a := range s.Card.Actions {
			if a.Label == label {
				return a.Token
			}
		}
	}
	t.Fatalf("missing %s for %s", label, to)
	return ""
}

func status(t *testing.T, ctx context.Context, conn *pgx.Conn, fx *fx, id string) string {
	t.Helper()
	view, ok, err := Get(ctx, conn, fx.tenant, id)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	return view["status"].(string)
}

func decisions(t *testing.T, ctx context.Context, conn *pgx.Conn, fx *fx, id string) int {
	t.Helper()
	view, ok, err := Get(ctx, conn, fx.tenant, id)
	if err != nil || !ok {
		t.Fatal(err)
	}
	list, _ := view["decisions"].([]map[string]any)
	return len(list)
}