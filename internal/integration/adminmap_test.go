package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/app/admin"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The read-only map on the admin booking detail.
//
// Worth testing through the router rather than by unit-testing the payload
// builder: the value of the block is that an operator dispatching a therapist
// can see whether the pin agrees with the address, and that only happens if the
// data reaches the rendered attributes. The Leaflet half is a browser check and
// lives in QA.md; what curl can prove is that the coordinates, the tile config
// and the link out are all in the markup.

// adminBookingDetail renders GET /admin/booking/{id} as the seeded admin.
func adminBookingDetail(t *testing.T, env *testsupport.Env, bookingID int64) string {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/booking/%d", bookingID), nil)
	cookie, _ := env.SignedIn(1) // the seeded admin
	r.AddCookie(cookie)

	rec := httptest.NewRecorder()
	admin.Routes(env.Deps).ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/booking/%d = %d, want 200", bookingID, rec.Code)
	}
	return rec.Body.String()
}

func TestAdminBookingDetailPreviewsTheMapPin(t *testing.T) {
	env := testsupport.New(t)

	booking := env.Booking(testsupport.BookingInput{
		UserID:    env.User(9700).ID,
		SlotID:    env.FutureSlot(3, 1).ID,
		Latitude:  testLat,
		Longitude: testLng,
	})

	body := adminBookingDetail(t, env, booking.ID)

	for _, want := range []string{
		`data-map`,
		// The flag that tells app.js not to build a picker here. Without it the
		// operator gets a draggable pin whose moves are never saved.
		`data-map-readonly`,
		// The preview has no hidden inputs, so the coordinates travel as data
		// attributes instead.
		`data-map-lat="` + testLat + `"`,
		`data-map-lng="` + testLng + `"`,
		// Verbatim, placeholders and all — see the booking form's version of this
		// assertion for why data-map-tiles is not data-map-tile-url.
		`data-map-tiles="` + env.Cfg.Map.TileURL + `"`,
		`data-map-canvas`,
		`Buka di Google Maps`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the admin booking detail does not carry %s", want)
		}
	}

	if strings.Contains(body, "%7b") || strings.Contains(body, "%7B") {
		t.Error("the tile template was percent-escaped; the placeholders are broken")
	}

	// The link out is the half that survives JavaScript being blocked, so it is
	// server-rendered markup and must carry both coordinates.
	if !strings.Contains(body, "google.com/maps") {
		t.Error("no Google Maps link rendered")
	}
	if !strings.Contains(body, "-0.9492400%2C100.3542700") {
		t.Errorf("the maps link does not carry the escaped coordinate pair")
	}

	// A preview is not a picker: none of the picker's controls belong here, and
	// a stray hidden input would make the operator's drag look like it saved.
	for _, unwanted := range []string{`data-map-detect`, `data-map-clear`, `data-map-lat-input`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the read-only preview rendered the picker control %s", unwanted)
		}
	}
}

// Most bookings have no pin — it is optional on the form — so the absent case is
// the ordinary one, and a map of the equator would be worse than no map.
func TestAdminBookingDetailOmitsTheMapWithoutAPin(t *testing.T) {
	env := testsupport.New(t)

	booking := env.Booking(testsupport.BookingInput{
		UserID: env.User(9701).ID,
		SlotID: env.FutureSlot(4, 1).ID,
	})

	body := adminBookingDetail(t, env, booking.ID)

	if strings.Contains(body, "data-map-canvas") {
		t.Error("a booking with no pin rendered a map")
	}
	if strings.Contains(body, "Buka di Google Maps") {
		t.Error("a booking with no pin rendered a maps link")
	}
	// The rest of the page must still be there — the map is a section, not the
	// page.
	if !strings.Contains(body, booking.BookingCode) {
		t.Error("the detail page did not render")
	}
}
