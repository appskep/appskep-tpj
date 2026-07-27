package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// TPJ owns no credentials, so an empty admin list is repaired by editing the
// database or the ADMIN_USER_IDS allowlist and logging in again. Both guards
// below exist because refusing is cheaper than explaining that.

func TestLastAdminGuard(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	other := env.User(8700)

	t.Run("the only admin cannot be demoted", func(t *testing.T) {
		_, err := env.Deps.Users.SetRole(context.Background(), other.ID, admin.ID, false)
		if !errors.Is(err, service.ErrLastAdmin) {
			t.Fatalf("demoting the last admin got %v, want ErrLastAdmin", err)
		}
		if got := adminCount(t, env); got != 1 {
			t.Errorf("%d active admins remain, want 1", got)
		}
	})

	t.Run("the only admin cannot be deactivated", func(t *testing.T) {
		_, err := env.Deps.Users.SetActive(context.Background(), other.ID, admin.ID, false)
		if !errors.Is(err, service.ErrLastAdmin) {
			t.Fatalf("deactivating the last admin got %v, want ErrLastAdmin", err)
		}
		if got := adminCount(t, env); got != 1 {
			t.Errorf("%d active admins remain, want 1", got)
		}
	})

	t.Run("with two admins, one may go", func(t *testing.T) {
		if _, err := env.Deps.Users.SetRole(context.Background(), admin.ID, other.ID, true); err != nil {
			t.Fatalf("promoting a second admin: %v", err)
		}
		if got := adminCount(t, env); got != 2 {
			t.Fatalf("%d active admins, want 2", got)
		}

		if _, err := env.Deps.Users.SetRole(context.Background(), other.ID, admin.ID, false); err != nil {
			t.Fatalf("demoting one of two admins: %v", err)
		}
		if got := adminCount(t, env); got != 1 {
			t.Errorf("%d active admins remain, want 1", got)
		}
	})
}

// TestSelfDemotionIsRefused. The same class of mistake as the last-admin guard,
// one step earlier: a single misclick would otherwise end the session that made
// it, mid-task.
func TestSelfDemotionIsRefused(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	second := env.User(8710)
	if _, err := env.Deps.Users.SetRole(context.Background(), admin.ID, second.ID, true); err != nil {
		t.Fatalf("promoting a second admin: %v", err)
	}

	// Two admins now, so the last-admin guard is not what refuses this.
	if got := adminCount(t, env); got != 2 {
		t.Fatalf("%d active admins, want 2", got)
	}

	_, err := env.Deps.Users.SetRole(context.Background(), admin.ID, admin.ID, false)
	if !errors.Is(err, service.ErrSelfDemotion) {
		t.Errorf("self-demotion got %v, want ErrSelfDemotion", err)
	}

	_, err = env.Deps.Users.SetActive(context.Background(), admin.ID, admin.ID, false)
	if !errors.Is(err, service.ErrSelfDemotion) {
		t.Errorf("self-deactivation got %v, want ErrSelfDemotion", err)
	}

	if got := adminCount(t, env); got != 2 {
		t.Errorf("%d active admins after two refused self-demotions, want 2", got)
	}
}

// TestDeactivatedAdminsDoNotCountTowardTheGuard: only an ACTIVE admin can reach
// the panel, so a deactivated one must not be what keeps the list from emptying.
func TestDeactivatedAdminsDoNotCountTowardTheGuard(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	spare := env.User(8720)
	actor := env.User(8721)

	if _, err := env.Deps.Users.SetRole(context.Background(), admin.ID, spare.ID, true); err != nil {
		t.Fatalf("promoting: %v", err)
	}
	if _, err := env.Deps.Users.SetActive(context.Background(), admin.ID, spare.ID, false); err != nil {
		t.Fatalf("deactivating the spare: %v", err)
	}

	// One ACTIVE admin remains, even though two rows carry the role.
	if got := adminCount(t, env); got != 1 {
		t.Fatalf("%d active admins, want 1", got)
	}

	if _, err := env.Deps.Users.SetRole(context.Background(), actor.ID, admin.ID, false); !errors.Is(err, service.ErrLastAdmin) {
		t.Errorf("demoting the last ACTIVE admin got %v, want ErrLastAdmin — a "+
			"deactivated admin cannot reach the panel and must not count", err)
	}
}

// TestRoleChangeIsIdempotent: the toggle posts the value it wants, not a flip, so
// a resent request settles on the state of the last click.
func TestRoleChangeIsIdempotent(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	user := env.User(8730)

	for range 3 {
		got, err := env.Deps.Users.SetRole(context.Background(), admin.ID, user.ID, true)
		if err != nil {
			t.Fatalf("SetRole: %v", err)
		}
		if got.Role != sqlc.UsersRoleAdmin {
			t.Errorf("role = %q, want admin", got.Role)
		}
	}
	for range 3 {
		got, err := env.Deps.Users.SetRole(context.Background(), admin.ID, user.ID, false)
		if err != nil {
			t.Fatalf("SetRole: %v", err)
		}
		if got.Role != sqlc.UsersRoleUser {
			t.Errorf("role = %q, want user", got.Role)
		}
	}
}

// TestSettingsWhitelist. The form is generated from EditableSettings and the
// WHITELIST is what Update iterates — never the request. A settings table is a
// key/value store, and a handler that wrote every posted field would let anyone
// who can reach the page invent keys, or overwrite one the code reads with a
// value it cannot parse.
func TestSettingsWhitelist(t *testing.T) {
	env := testsupport.New(t)

	before := env.CountRows("settings", "")

	err := env.Deps.Settings.Update(context.Background(), map[string]string{
		service.KeySiteName: "TPJ Baru",
		// Not in EditableSettings. An extra field is a stale form, not an attack
		// worth a page about, so it is ignored rather than refused.
		"invented_key":     "nilai",
		"database_url":     "mysql://…",
		"payment_provider": "gratis",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := env.Deps.Settings.String(service.KeySiteName, ""); got != "TPJ Baru" {
		t.Errorf("site_name = %q, want the written value", got)
	}
	if got := env.CountRows("settings", ""); got != before {
		t.Errorf("%d settings rows, want %d — an unlisted key was written", got, before)
	}
	for _, key := range []string{"invented_key", "database_url", "payment_provider"} {
		if got := env.CountRows("settings", "setting_key = ?", key); got != 0 {
			t.Errorf("the unlisted key %q was written", key)
		}
	}
}

// TestSettingsUpdateLeavesAbsentKeysAlone: an absent key is left as it is rather
// than blanked, so a partial form cannot wipe the contact block.
func TestSettingsUpdateLeavesAbsentKeysAlone(t *testing.T) {
	env := testsupport.New(t)

	tagline := env.Deps.Settings.String(service.KeySiteTagline, "")
	if tagline == "" {
		t.Fatal("the seeded tagline is empty; this test needs a value to preserve")
	}

	if err := env.Deps.Settings.Update(context.Background(), map[string]string{
		service.KeySiteName: "Hanya nama",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := env.Deps.Settings.String(service.KeySiteTagline, ""); got != tagline {
		t.Errorf("the tagline became %q; an absent key must be left alone", got)
	}
}

// TestSettingsUpdateValidatesNumerics. The bounds mirror what the readers of each
// key actually tolerate — a lead time of "dua jam" would silently fall back to
// the default on every render instead of being refused where it was typed.
func TestSettingsUpdateValidatesNumerics(t *testing.T) {
	env := testsupport.New(t)

	before := env.Deps.Settings.Int(service.KeyBookingLeadMinutes, 0)

	err := env.Deps.Settings.Update(context.Background(), map[string]string{
		service.KeyBookingLeadMinutes: "dua jam",
	})

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Update got %v, want a ValidationError", err)
	}
	if _, ok := ve.Fields[service.KeyBookingLeadMinutes]; !ok {
		t.Errorf("messages = %v, want one on the lead time", ve.Fields)
	}
	// Nothing was written: the whole form is validated before any of it commits.
	if got := env.Deps.Settings.Int(service.KeyBookingLeadMinutes, 0); got != before {
		t.Errorf("lead time = %d, want the unchanged %d", got, before)
	}
}

// TestSettingsReachTheAppOnTheNextRequest. Settings.Reload runs after the commit,
// so an edit takes effect without a restart — and the booking window is read
// through the same cache.
func TestSettingsReachTheAppImmediately(t *testing.T) {
	env := testsupport.New(t)

	_, until := env.Deps.Schedule.Window()
	before := until

	if err := env.Deps.Settings.Update(context.Background(), map[string]string{
		service.KeyBookingMaxDaysAhead: "7",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	_, until = env.Deps.Schedule.Window()
	if !until.Before(before) {
		t.Errorf("the booking window still ends at %v after the max-days setting was "+
			"cut to 7 (was %v) — Reload did not run", until, before)
	}
	if got := until.Sub(env.Deps.Schedule.Today()).Hours() / 24; got != 7 {
		t.Errorf("the window is %v days, want 7", got)
	}
}

func adminCount(t *testing.T, env *testsupport.Env) int {
	t.Helper()
	return env.CountRows("users", "role = 'admin' AND is_active = 1")
}
