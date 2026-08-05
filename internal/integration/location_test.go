package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/app/public"
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The map pin and the prefill chain.
//
// The pin is optional everywhere, which is what makes these worth writing: an
// optional field that silently fails to save looks exactly like a customer who
// chose not to fill it in, and nothing downstream would complain.

const (
	testLat = "-7.7970680"
	testLng = "110.3705290"
)

// getBooking renders GET /booking at step 3 as a signed-in user, and returns the
// HTML.
//
// Through the real router rather than by calling the handler directly: the
// prefill runs inside Form, and what has to be proven is that it reaches the
// rendered form's value= attributes — not that a function returned a struct.
//
// The query string is what unlocks step 3, and step 3 is where the data form
// and the map live: a bare /booking renders the layanan picker and nothing this
// file cares about. It is also how the real page is reached — the flow is one
// URL carrying its state in the query string.
func getBooking(t *testing.T, env *testsupport.Env, appskepID uint64, slot sqlc.ScheduleSlot) string {
	t.Helper()

	target := "/booking?layanan=" + testsupport.SlugUrut +
		"&tanggal=" + slot.SlotDate.Format("2006-01-02") +
		"&slot=" + fmt.Sprint(slot.ID)

	r := httptest.NewRequest(http.MethodGet, target, nil)
	cookie, _ := env.SignedIn(appskepID)
	r.AddCookie(cookie)

	rec := httptest.NewRecorder()
	public.Routes(env.Deps).ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /booking = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// confirmForm returns just the review panel's commit form — the one carrying
// konfirmasi=1, which is the form that actually writes the booking.
func confirmForm(t *testing.T, body string) string {
	t.Helper()

	start := strings.Index(body, `name="konfirmasi" value="1"`)
	if start < 0 {
		t.Fatal("the review panel did not render")
	}
	// Back to the <form that opens it, forward to the </form> that closes it.
	open := strings.LastIndex(body[:start], "<form")
	end := strings.Index(body[start:], "</form>")
	if open < 0 || end < 0 {
		t.Fatal("the confirm control is not inside a form")
	}
	return body[open : start+end]
}

// assertInputValue finds `name="<field>"` and checks the value= beside it. Crude
// on purpose: parsing the document would let a test pass against markup no
// browser would submit.
func assertInputValue(t *testing.T, body, field, want string) {
	t.Helper()

	needle := `name="` + field + `" value="` + want + `"`
	if !strings.Contains(body, needle) {
		t.Errorf("the form does not carry %s", needle)
	}
}

// TestBookingStoresTheMapPin is the round trip: a pin submitted through
// Booking.Create comes back out of the detail query unchanged, normalised to the
// seven decimal places the column holds.
func TestBookingStoresTheMapPin(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9401)
	slot := env.FutureSlot(3, 1)

	booking := env.Booking(testsupport.BookingInput{
		UserID: user.ID,
		SlotID: slot.ID,
		// Deliberately under-precise on the way in: the service normalises to 7 dp,
		// so what comes back out must be padded rather than left as typed.
		Latitude:  "-7.797068",
		Longitude: "110.370529",
	})

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, user.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	if !detail.Latitude.Valid || !detail.Longitude.Valid {
		t.Fatal("the stored booking carries no pin")
	}
	if detail.Latitude.String != testLat || detail.Longitude.String != testLng {
		t.Errorf("pin = (%q,%q), want (%q,%q)",
			detail.Latitude.String, detail.Longitude.String, testLat, testLng)
	}

	env.AssertInvariant()
}

// TestBookingWithoutAMapPinStoresNull. The ordinary case — a browser that
// refused geolocation, or a customer who never touched the map. It must be
// distinguishable from a pin, not stored as 0,0.
func TestBookingWithoutAMapPinStoresNull(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9402)
	slot := env.FutureSlot(3, 1)

	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, user.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	if detail.Latitude.Valid || detail.Longitude.Valid {
		t.Errorf("a booking with no pin stored (%v,%v)", detail.Latitude, detail.Longitude)
	}

	env.AssertInvariant()
}

// TestBookingRefusesHalfAPin. The hidden inputs are written by a script, so this
// is what a truncated submission or a tampered form looks like — and half a pin
// is a location nobody can drive to.
func TestBookingRefusesHalfAPin(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9403)
	slot := env.FutureSlot(3, 1)

	_, err := env.Deps.Booking.Create(context.Background(), service.CreateInput{
		UserID:      user.ID,
		ServiceSlug: testsupport.SlugUrut,
		SlotID:      fmt.Sprint(slot.ID),
		Name:        "Budi Santoso",
		Phone:       "081234567890",
		Address:     "Jl. Contoh No. 1",
		Latitude:    "-7.797068",
	})

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Create accepted half a pin: %v", err)
	}
	if _, ok := ve.Fields["lokasi"]; !ok {
		t.Errorf("messages = %v, want one on \"lokasi\"", ve.Fields)
	}

	// Refused before the slot was touched: a rejected form must not leave a hold
	// behind, which is the whole reason validate runs before the transaction.
	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d after a rejected booking, want 0", got)
	}

	env.AssertInvariant()
}

// TestProfileStoresAndClearsTheMapPin. The profile is the other half of "both or
// neither": clearing the pin has to be reachable, or a customer who once allowed
// geolocation can never take the saved location back.
func TestProfileStoresAndClearsTheMapPin(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9404)

	saved, err := env.Deps.Profile.Update(context.Background(), user.ID, service.ProfileInput{
		Phone:     "081234567890",
		Address:   "Jl. Contoh No. 1",
		Latitude:  "-7.797068",
		Longitude: "110.370529",
	})
	if err != nil {
		t.Fatalf("Profile.Update: %v", err)
	}
	if saved.Latitude != testLat || saved.Longitude != testLng {
		t.Errorf("saved pin = (%q,%q), want (%q,%q)",
			saved.Latitude, saved.Longitude, testLat, testLng)
	}

	cleared, err := env.Deps.Profile.Update(context.Background(), user.ID, service.ProfileInput{
		Phone:   "081234567890",
		Address: "Jl. Contoh No. 1",
	})
	if err != nil {
		t.Fatalf("Profile.Update clearing the pin: %v", err)
	}
	if cleared.Latitude != "" || cleared.Longitude != "" {
		t.Errorf("the pin survived being cleared: (%q,%q)", cleared.Latitude, cleared.Longitude)
	}
}

// TestBookingFormPrefillsFromTheLastBooking is the feature: a customer who never
// opened /profil should not re-type their phone, address and pin for every visit.
func TestBookingFormPrefillsFromTheLastBooking(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9405)
	env.Booking(testsupport.BookingInput{
		UserID:    user.ID,
		SlotID:    env.FutureSlot(3, 1).ID,
		Phone:     "081298765432",
		Address:   "Jl. Kaliurang KM 7 No. 3, Sleman",
		Latitude:  "-7.797068",
		Longitude: "110.370529",
	})

	// A different slot for the next booking: the one above is full, and the form
	// would drop back to step 2 rather than render the fields under test.
	body := getBooking(t, env, user.AppskepUserID, env.FutureSlot(4, 1))

	assertInputValue(t, body, "telepon", "081298765432")
	assertInputValue(t, body, "latitude", testLat)
	assertInputValue(t, body, "longitude", testLng)
	// The address is a textarea, so it has no value= to look for.
	if !strings.Contains(body, "Jl. Kaliurang KM 7 No. 3, Sleman") {
		t.Error("the form does not carry the previous booking's address")
	}
}

// TestTheProfileWinsOverTheLastBooking. The profile is the page a customer went
// to on purpose, so what they saved there must not be overwritten by whatever
// they typed into a booking form afterwards — and the fallback is per field, not
// all or nothing.
func TestTheProfileWinsOverTheLastBooking(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9406)
	env.Booking(testsupport.BookingInput{
		UserID:    user.ID,
		SlotID:    env.FutureSlot(3, 1).ID,
		Phone:     "081298765432",
		Address:   "Jl. Kaliurang KM 7 No. 3, Sleman",
		Latitude:  "-7.797068",
		Longitude: "110.370529",
	})

	// A profile holding the phone number and nothing else: the phone must come
	// from here, the address and the pin from the booking above.
	if _, err := env.Deps.Profile.Update(context.Background(), user.ID, service.ProfileInput{
		Phone: "081200000000",
	}); err != nil {
		t.Fatalf("Profile.Update: %v", err)
	}

	body := getBooking(t, env, user.AppskepUserID, env.FutureSlot(4, 1))

	assertInputValue(t, body, "telepon", "081200000000")
	assertInputValue(t, body, "latitude", testLat)
	if !strings.Contains(body, "Jl. Kaliurang KM 7 No. 3, Sleman") {
		t.Error("an address saved in no profile was not taken from the last booking")
	}
}

// TestBookingFormForAFirstTimeCustomer. No profile, no previous booking: an
// empty form and no error. The lookup returns sql.ErrNoRows, which is the
// ordinary case for every new customer and must not read as a failure.
func TestBookingFormForAFirstTimeCustomer(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9407)

	contact, err := env.Deps.Booking.LastContact(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("LastContact for a customer with no bookings: %v", err)
	}
	if contact != (service.BookingContact{}) {
		t.Errorf("LastContact invented %+v for a customer with no bookings", contact)
	}

	body := getBooking(t, env, user.AppskepUserID, env.FutureSlot(3, 1))

	assertInputValue(t, body, "latitude", "")
	assertInputValue(t, body, "longitude", "")
	// The name still comes from Appskep, so the form is not simply blank.
	if !strings.Contains(body, `name="nama"`) {
		t.Error("the form did not render")
	}
}

// TestTheReviewPanelRePostsTheMapPin guards the markup, because nothing else
// can.
//
// The booking form is two POSTs to one URL: the first answers with a review
// panel, the second commits. They are SEPARATE forms — the map widget lives in
// the first and the second re-posts every value as a hidden input. Leave the
// coordinates out of that second form and the review shows a location the commit
// silently discards: no error, no log line, and a service-level test that calls
// Create directly never touches the path at all.
func TestTheReviewPanelRePostsTheMapPin(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9409)
	slot := env.FutureSlot(3, 1)
	cookie, csrf := env.SignedIn(user.AppskepUserID)

	form := url.Values{
		"_csrf":     {csrf},
		"layanan":   {testsupport.SlugUrut},
		"tanggal":   {slot.SlotDate.Format("2006-01-02")},
		"slot":      {fmt.Sprint(slot.ID)},
		"nama":      {"Budi Santoso"},
		"telepon":   {"081234567890"},
		"alamat":    {"Jl. Contoh No. 1"},
		"latitude":  {"-7.797068"},
		"longitude": {"110.370529"},
	}

	r := httptest.NewRequest(http.MethodPost, "/booking", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)

	rec := httptest.NewRecorder()
	public.Routes(env.Deps).ServeHTTP(rec, r)

	// 200, not a redirect: the review's state is the customer's name, phone and
	// address, and none of that may travel in a URL. This is also why the form
	// carries data-turbo="false".
	if rec.Code != http.StatusOK {
		t.Fatalf("the review POST = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Scoped to the confirm form, not the whole page.
	//
	// This matters more than it looks: the map widget in the FIRST form carries
	// hidden inputs with the same two names, so an assertion against the whole
	// body passes whether or not the confirm form has them — which is to say it
	// tests nothing at all. Verified by deleting the inputs and watching the
	// unscoped version stay green.
	body := confirmForm(t, rec.Body.String())
	// The SUBMITTED strings, not the normalised ones. bookingFormValues holds
	// what the browser sent verbatim — the rule that lets a 422 re-render exactly
	// what the user typed — so the confirm form re-posts that and Create
	// normalises again on the commit. Asserting on the normalised form here would
	// be asserting on a behaviour the codebase deliberately does not have.
	assertInputValue(t, body, "latitude", "-7.797068")
	assertInputValue(t, body, "longitude", "110.370529")

	// Nothing written yet — the review is a dry run.
	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d after a review, want 0", got)
	}

	env.AssertInvariant()
}

// TestTheMapWidgetIsConfiguredFromTheEnvironment. The script reads the tile
// server out of these attributes and holds no copy of its own, so an empty one
// is a blank map — and the same setting is what the CSP's img-src is built from.
func TestTheMapWidgetIsConfiguredFromTheEnvironment(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9408)
	body := getBooking(t, env, user.AppskepUserID, env.FutureSlot(3, 1))

	for _, want := range []string{
		`data-map`,
		// Verbatim, placeholders and all. html/template URL-normalises any
		// attribute whose name ends in "url", which turned {s} into %7bs%7d and
		// pointed every tile request at a host that does not exist — a grey map
		// and correct-looking markup. Hence data-map-tiles, and hence this
		// assertion comparing against the configured string exactly.
		`data-map-tiles="` + env.Cfg.Map.TileURL + `"`,
		`data-map-default-lat="` + env.Cfg.Map.DefaultLat + `"`,
		`data-map-canvas`,
		`data-map-detect`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the booking form does not carry %s", want)
		}
	}
	if strings.Contains(body, "%7b") || strings.Contains(body, "%7B") {
		t.Error("the tile template was percent-escaped; the placeholders are broken")
	}

	// The detect button ships hidden and is revealed by app.js. Without this the
	// no-JS path shows a control that cannot do anything.
	if !strings.Contains(body, "data-map-detect hidden") {
		t.Error("the detect button is not hidden for a visitor with no JavaScript")
	}
}
