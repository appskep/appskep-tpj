package view

import "testing"

// navItem finds a nav entry by href so the tests exercise the real menus rather
// than literals that could drift from them.
func navItem(t *testing.T, items []NavItem, href string) NavItem {
	t.Helper()
	for _, it := range items {
		if it.Href == href {
			return it
		}
	}
	t.Fatalf("no nav item with href %q", href)
	return NavItem{}
}

func TestActiveNav(t *testing.T) {
	dasbor := navItem(t, adminNav, "/admin")
	booking := navItem(t, adminNav, "/admin/booking")
	pengaturan := navItem(t, adminNav, "/admin/pengaturan")
	layanan := navItem(t, publicNav, "/layanan")

	tests := []struct {
		name    string
		current string
		item    NavItem
		want    bool
	}{
		// A section index matches only itself.
		{"dasbor on its own page", "/admin", dasbor, true},
		{"dasbor with trailing slash", "/admin/", dasbor, true},
		{"dasbor on a sibling page", "/admin/booking", dasbor, false},
		{"dasbor on a nested page", "/admin/layanan/1/edit", dasbor, false},

		// Every other item still matches its own subtree.
		{"booking on its list", "/admin/booking", booking, true},
		{"booking on a detail page", "/admin/booking/TPJ-20260728-A7K2", booking, true},
		{"pengaturan on its page", "/admin/pengaturan", pengaturan, true},
		{"booking on pengaturan", "/admin/pengaturan", booking, false},

		// A shared prefix that is not a path segment must not match.
		{"booking is not a prefix match", "/admin/bookingan", booking, false},

		// Public nav keeps the subtree rule.
		{"layanan on a detail page", "/layanan/urut-therapeutic", layanan, true},
		{"layanan elsewhere", "/riwayat", layanan, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := activeNav(tc.current, tc.item); got != tc.want {
				t.Errorf("activeNav(%q, %q) = %v, want %v", tc.current, tc.item.Href, got, tc.want)
			}
		})
	}
}

// Exactly one item may be current on any admin page — the defect this guards is
// two highlighted links, not a wrong one.
func TestAdminNavHasOneActiveItem(t *testing.T) {
	paths := []string{
		"/admin",
		"/admin/booking",
		"/admin/booking/TPJ-20260728-A7K2",
		"/admin/jadwal",
		"/admin/jadwal/generate",
		"/admin/layanan",
		"/admin/layanan/1/edit",
		"/admin/pembayaran",
		"/admin/users",
		"/admin/pengaturan",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			var active []string
			for _, item := range adminNav {
				if activeNav(path, item) {
					active = append(active, item.Href)
				}
			}
			if len(active) != 1 {
				t.Errorf("%s: %d active items %v, want exactly 1", path, len(active), active)
			}
		})
	}
}
