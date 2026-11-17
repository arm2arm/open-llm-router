package integration_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/settings"
)

func TestLegacyRoutingRollbackKeepsDeploymentAdministratorsKeys(t *testing.T) {
	database := openDatabase(t, newDatabase(t).dsn)
	cfg, err := config.Parse([]byte("models: {}\ngroups:\n  admins: {models: ['*'], admin: true}\n"), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	store := auth.Admin{Pool: database}
	owner, err := store.AddUser(t.Context(), "deployment-admin", "", []string{"admins"})
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := store.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ChangeLocalPassword(t.Context(), root.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Type: "operator_cli", Name: "legacy-routing-test"}
	issue := func(id string) (adminaccess.Key, string) {
		t.Helper()
		c, e := adminaccess.Validate(adminaccess.Create{Label: "routing fixture", Days: 1, Permissions: []string{"routing:read"}, Networks: []string{"127.0.0.1"}}, 90)
		if e != nil {
			t.Fatal(e)
		}
		k, raw, e := adminaccess.Issue(t.Context(), database, id, c, nil, 10, actor)
		if e != nil {
			t.Fatal(e)
		}
		return k, raw
	}
	s := settings.RoutingService{DB: database, File: cfg}
	v, err := s.Import(t.Context(), settings.DomainRouting, actor)
	if err != nil {
		t.Fatal(err)
	}
	key, raw := issue(owner)
	_, protectedRaw := issue(root.ID)
	for _, groupsNull := range []bool{false, true} {
		// Seed the stored shape produced before managed groups existed. The immutable
		// historical revision is then restored through the normal rollback service.
		legacy := map[string]any{"models": cfg.Models, "aliases": cfg.Aliases}
		if groupsNull {
			legacy["groups"] = nil
		}
		data, _ := json.Marshal(legacy)
		var id int64
		if err := database.QueryRow(t.Context(), `INSERT INTO settings_revisions(domain,origin,document,created_by_label) VALUES('routing','import',$1,'legacy fixture') RETURNING id`, data).Scan(&id); err != nil {
			t.Fatal(err)
		}
		execSQL(t, database, `INSERT INTO settings_activations(domain,revision_id,kind,activated_by_label) VALUES('routing',$1,'import','legacy fixture')`, id)
		v, err = s.Rollback(t.Context(), settings.DomainRouting, v.Version, id, actor)
		if err != nil {
			t.Fatal(err)
		}
		if !v.Active.Document.Groups["admins"].Admin {
			t.Fatal("legacy policy did not resolve to deployment")
		}
		if _, err := adminaccess.Lookup(t.Context(), database, raw); err != nil {
			t.Fatal("legacy rollback revoked administrator credential", err)
		}
	}
	// A real policy removal must still revoke non-protected administrators and
	// record its reason in the same transaction; protected recovery remains.
	doc := *v.Active.Document
	group := doc.Groups["admins"]
	group.Admin = false
	doc.Groups["admins"] = group
	data, _ := json.Marshal(doc)
	v, err = s.SaveDraft(t.Context(), settings.DomainRouting, v.Version, data, nil, actor)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Activate(t.Context(), settings.DomainRouting, v.Version, v.Draft.ID, actor, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminaccess.Lookup(t.Context(), database, raw); !errors.Is(err, adminaccess.ErrDenied) {
		t.Fatal("actual demotion kept credential", err)
	}
	if _, err := adminaccess.Lookup(t.Context(), database, protectedRaw); err != nil {
		t.Fatal("protected recovery key revoked", err)
	}
	var count int
	if err := database.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='admin_key.revoke' AND target_id=$1 AND reason='owner no longer has administration authority'`, key.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("policy revocation lacks one audit event", count, err)
	}
}
