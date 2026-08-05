package testsupport

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/service"
)

// SeededSlugs are the three layanan seed_dev.sql creates. Ids are auto-increment
// and change on every reset, so a fixture resolves a service by slug and never by
// id — the same reason the seed file says so itself.
const (
	SlugUrut    = "urut-therapeutic"
	SlugMassage = "massage-therapeutic"
	SlugBekam   = "bekam-therapeutic"
)

// Service returns a seeded layanan by slug.
func (e *Env) Service(slug string) sqlc.Service {
	e.T.Helper()

	svc, err := e.Store.Queries.GetServiceBySlug(context.Background(), slug)
	if err != nil {
		e.T.Fatalf("testsupport: service %q: %v", slug, err)
	}
	return svc
}

// User creates (or refreshes) a local user mirror for an Appskep id and returns
// it.
//
// Through UpsertUserFromSSO, the same query the real login uses, so a fixture
// user is indistinguishable from one that signed in — including having
// last_login_at stamped.
func (e *Env) User(appskepID uint64) sqlc.User {
	e.T.Helper()
	ctx := context.Background()

	if _, err := e.Store.Queries.UpsertUserFromSSO(ctx, sqlc.UpsertUserFromSSOParams{
		AppskepUserID: appskepID,
		Email:         fmt.Sprintf("user%d@example.test", appskepID),
		Name:          fmt.Sprintf("Pengguna %d", appskepID),
	}); err != nil {
		e.T.Fatalf("testsupport: upserting user %d: %v", appskepID, err)
	}

	u, err := e.Store.Queries.GetUserByAppskepID(ctx, appskepID)
	if err != nil {
		e.T.Fatalf("testsupport: reading user %d: %v", appskepID, err)
	}
	return u
}

// UserModel returns a local user as the view and service layers speak it.
//
// Payment.Start takes a *model.User rather than a sqlc row, because that is what
// the request context carries — so a test driving a payment needs the flattened
// shape, not the generated one.
func (e *Env) UserModel(id int64) model.User {
	e.T.Helper()

	row, err := e.Store.Queries.GetUser(context.Background(), id)
	if err != nil {
		e.T.Fatalf("testsupport: reading user %d: %v", id, err)
	}
	return model.UserFromSQLC(row)
}

// Admin returns the seeded admin (appskep_user_id = 1).
func (e *Env) Admin() sqlc.User {
	e.T.Helper()

	u, err := e.Store.Queries.GetUserByAppskepID(context.Background(), 1)
	if err != nil {
		e.T.Fatalf("testsupport: reading seeded admin: %v", err)
	}
	if u.Role != sqlc.UsersRoleAdmin {
		e.T.Fatalf("testsupport: seeded user 1 has role %q, want admin", u.Role)
	}
	return u
}

// slotClock is where minted slots start, in minutes past midnight. 20:00 is
// outside the seeded 08:00-18:00 band, so a fixture slot never collides with a
// seeded one on uq_slots_date_start.
const slotClock = 20 * 60

// FutureSlot creates a bookable slot and returns it.
//
// daysAhead is deliberately a parameter rather than a constant: the booking
// window's two bounds (a 120-minute lead time and 30 days ahead) both need
// testing, and a fixture that could only produce one distance would not reach
// either. Three days out is the safe default — far enough past the lead time that
// the test cannot fail at 23:00, near enough to stay inside the window.
func (e *Env) FutureSlot(daysAhead, capacity int) sqlc.ScheduleSlot {
	e.T.Helper()
	return e.slotAt(daysAhead, slotClock, capacity)
}

// SlotAt is FutureSlot with an explicit start time in minutes past midnight, for
// the tests that need two distinct slots on the same day.
func (e *Env) SlotAt(daysAhead, startMinutes, capacity int) sqlc.ScheduleSlot {
	e.T.Helper()
	return e.slotAt(daysAhead, startMinutes, capacity)
}

func (e *Env) slotAt(daysAhead, startMinutes, capacity int) sqlc.ScheduleSlot {
	e.T.Helper()
	ctx := context.Background()

	day := time.Now().In(e.Cfg.App.Location).AddDate(0, 0, daysAhead)
	date := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, e.Cfg.App.Location)

	const duration = 150 // the seeded slot_default_duration_minutes
	start := clock(startMinutes)
	end := clock(startMinutes + duration)

	res, err := e.Store.Queries.CreateSlot(ctx, sqlc.CreateSlotParams{
		SlotDate:  date,
		StartTime: start,
		EndTime:   end,
		Capacity:  int32(capacity),
		IsActive:  true,
	})
	if err != nil {
		e.T.Fatalf("testsupport: creating slot %s %s: %v", date.Format(time.DateOnly), start, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		e.T.Fatalf("testsupport: slot id: %v", err)
	}

	slot, err := e.Store.Queries.GetSlot(ctx, id)
	if err != nil {
		e.T.Fatalf("testsupport: reading slot %d: %v", id, err)
	}
	return slot
}

func clock(minutes int) string {
	return fmt.Sprintf("%02d:%02d:00", minutes/60, minutes%60)
}

// BookingInput is the shape a fixture booking is created from. Zero fields get
// working defaults, so a test names only what it cares about.
type BookingInput struct {
	UserID  int64
	SlotID  int64
	Slug    string
	Name    string
	Phone   string
	Address string
	// Latitude and Longitude are the map pin, and are the one pair here with NO
	// default: the pin is optional on the form, so a fixture that did not ask for
	// one must produce a booking without one — that is the ordinary case and the
	// one most code paths have to handle.
	Latitude  string
	Longitude string
	Notes     string
}

// Booking creates a booking through service.Booking.Create.
//
// Through the application, never by INSERT — seed_dev.sql:84-87 gives the reason:
// a hand-written booking either violates the booked_count invariant or has to be
// hand-maintained. Going through Create also means every fixture booking has been
// past the same lock, the same validation and the same code generator as a real
// one.
func (e *Env) Booking(in BookingInput) sqlc.Booking {
	e.T.Helper()

	if in.Slug == "" {
		in.Slug = SlugUrut
	}
	if in.Name == "" {
		in.Name = "Budi Santoso"
	}
	if in.Phone == "" {
		in.Phone = "081234567890"
	}
	// Required since the copy fix: the therapist travels to this address.
	if in.Address == "" {
		in.Address = "Jl. Kaliurang KM 5 No. 12, Sleman"
	}

	booking, err := e.Deps.Booking.Create(context.Background(), service.CreateInput{
		UserID:      in.UserID,
		ServiceSlug: in.Slug,
		SlotID:      fmt.Sprint(in.SlotID),
		Name:        in.Name,
		Phone:       in.Phone,
		Address:     in.Address,
		Latitude:    in.Latitude,
		Longitude:   in.Longitude,
		Notes:       in.Notes,
	})
	if err != nil {
		e.T.Fatalf("testsupport: creating booking on slot %d: %v", in.SlotID, err)
	}
	return booking
}

// ---------------------------------------------------------------------------
// Direct database reads and writes
//
// Everything below reaches past the service layer on purpose. A test needs to
// observe state the application has no query for (does booked_count agree with
// the bookings?) and to create state the application cannot reach (a booking
// whose hold ran out an hour ago). Both go through Store.DB(), which is
// documented as existing for tests.
// ---------------------------------------------------------------------------

// BookedCount reads a slot's counter.
func (e *Env) BookedCount(slotID int64) int32 {
	e.T.Helper()

	var n int32
	if err := e.Store.DB().QueryRowContext(context.Background(),
		"SELECT booked_count FROM schedule_slots WHERE id = ?", slotID).Scan(&n); err != nil {
		e.T.Fatalf("testsupport: reading booked_count for slot %d: %v", slotID, err)
	}
	return n
}

// BookingStatus reads a booking's status.
func (e *Env) BookingStatus(bookingID int64) sqlc.BookingsStatus {
	e.T.Helper()

	var s string
	if err := e.Store.DB().QueryRowContext(context.Background(),
		"SELECT status FROM bookings WHERE id = ?", bookingID).Scan(&s); err != nil {
		e.T.Fatalf("testsupport: reading status for booking %d: %v", bookingID, err)
	}
	return sqlc.BookingsStatus(s)
}

// Exec runs a statement against the test database.
func (e *Env) Exec(query string, args ...any) {
	e.T.Helper()

	if _, err := e.Store.DB().ExecContext(context.Background(), query, args...); err != nil {
		e.T.Fatalf("testsupport: exec %.60q: %v", query, err)
	}
}

// CountRows is a one-line SELECT COUNT(*) for the "nothing was written" checks.
func (e *Env) CountRows(table, where string, args ...any) int {
	e.T.Helper()

	q := "SELECT COUNT(*) FROM " + table
	if where != "" {
		q += " WHERE " + where
	}

	var n int
	if err := e.Store.DB().QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		e.T.Fatalf("testsupport: %s: %v", q, err)
	}
	return n
}

// ExpireHold pushes a booking's payment deadline into the past, so the expiry
// ticker will pick it up on its next sweep.
//
// The only way to reach that state without waiting an hour. It writes expires_at
// alone and leaves the status untouched, which is exactly the state a real
// abandoned checkout is in when the sweep finds it.
func (e *Env) ExpireHold(bookingID int64) {
	e.T.Helper()
	e.Exec("UPDATE bookings SET expires_at = DATE_SUB(NOW(), INTERVAL 1 MINUTE) WHERE id = ?", bookingID)
}

// AssertInvariant is the assertion every DB-backed test ends with.
//
// It is the invariant CLAUDE.md names as the one the whole locking design exists
// to hold: booked_count equals the number of bookings on that slot that have not
// released it, and the counter never leaves its own bounds. Every phase from 1 to
// 12 checked this by hand at the end of its verification pass; this is that check
// made permanent.
func (e *Env) AssertInvariant() {
	e.T.Helper()
	AssertInvariant(e.T, e)
}

// AssertInvariant is the package-level form, for a test holding an Env in a local
// variable of its own name.
func AssertInvariant(t *testing.T, e *Env) {
	t.Helper()

	rows, err := e.Store.DB().QueryContext(context.Background(), `
		SELECT s.id, s.capacity, s.booked_count,
		       (SELECT COUNT(*) FROM bookings b
		         WHERE b.slot_id = s.id
		           AND b.status NOT IN ('cancelled','expired')) AS holding
		FROM schedule_slots s`)
	if err != nil {
		t.Fatalf("testsupport: invariant query: %v", err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var id int64
		var capacity, booked, holding int32
		if err := rows.Scan(&id, &capacity, &booked, &holding); err != nil {
			t.Fatalf("testsupport: invariant scan: %v", err)
		}
		checked++

		if booked != holding {
			t.Errorf("slot %d: booked_count = %d but %d bookings still hold it "+
				"(the booked_count invariant is broken)", id, booked, holding)
		}
		if booked < 0 || booked > capacity {
			t.Errorf("slot %d: booked_count = %d outside 0..capacity(%d)", id, booked, capacity)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("testsupport: invariant rows: %v", err)
	}
	if checked == 0 {
		t.Fatal("testsupport: invariant checked zero slots — the seed did not load")
	}
}

// ---------------------------------------------------------------------------
// Authentication and webhook helpers
// ---------------------------------------------------------------------------

// TokenOption adjusts the claims of a minted token.
type TokenOption func(jwt.MapClaims)

// WithExpiredAt sets the expired_at claim, which is what drives the inline
// refresh branch when it is in the past.
func WithExpiredAt(t time.Time) TokenOption {
	return func(c jwt.MapClaims) { c["expired_at"] = t.Unix() }
}

// WithClaim sets an arbitrary claim, for the malformed-claim cases.
func WithClaim(key string, value any) TokenOption {
	return func(c jwt.MapClaims) { c[key] = value }
}

// WithoutClaim removes a claim.
func WithoutClaim(key string) TokenOption {
	return func(c jwt.MapClaims) { delete(c, key) }
}

// MintToken signs an Appskep-shaped JWT with the given secret.
//
// The callback is the only way into this application's session, so there is no
// route to an authenticated request that does not start here. Phase 3 used a
// throwaway command for exactly this; it is a fixture now.
func MintToken(t *testing.T, secret string, appskepID uint64, opts ...TokenOption) string {
	t.Helper()

	claims := jwt.MapClaims{
		"user_id":    float64(appskepID),
		"email":      fmt.Sprintf("user%d@example.test", appskepID),
		"name":       fmt.Sprintf("Pengguna %d", appskepID),
		"expired_at": time.Now().Add(time.Hour).Unix(),
	}
	for _, opt := range opts {
		opt(claims)
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("testsupport: signing token: %v", err)
	}
	return signed
}

// SessionCookie builds the cookie a signed-in request carries.
//
// It goes through SessionManager.Save into a recorder and lifts the result,
// because the signing function is unexported — which is correct, and means a test
// cannot accidentally assert against a codec of its own invention.
func SessionCookie(t *testing.T, mgr *auth.SessionManager, s auth.Session) *http.Cookie {
	t.Helper()

	rec := httptest.NewRecorder()
	if err := mgr.Save(rec, s); err != nil {
		t.Fatalf("testsupport: saving session: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("testsupport: expected 1 session cookie, got %d", len(cookies))
	}
	return cookies[0]
}

// SignedIn returns the cookie for an authenticated session holding a fresh token
// for appskepID, plus a CSRF secret so a POST can carry a valid token.
func (e *Env) SignedIn(appskepID uint64) (*http.Cookie, string) {
	e.T.Helper()

	secret, err := auth.NewCSRFSecret()
	if err != nil {
		e.T.Fatalf("testsupport: csrf secret: %v", err)
	}

	cookie := SessionCookie(e.T, e.Deps.Session, auth.Session{
		Token: MintToken(e.T, e.Cfg.Auth.Secret, appskepID),
		CSRF:  secret,
	})
	return cookie, auth.MaskToken(secret)
}

// SignNotification builds a Midtrans notification body with a correct signature.
//
// One implementation, used by every payment test, so the SHA512 is computed the
// same way on both sides and a test can never pass because it made the same
// mistake twice. gross_amount is placed into the digest exactly as it appears in
// the body, which is PLAN.md R6's whole point.
func SignNotification(orderID, statusCode, grossAmount, serverKey string, extra map[string]any) []byte {
	sum := sha512.Sum512([]byte(orderID + statusCode + grossAmount + serverKey))

	payload := map[string]any{
		"order_id":      orderID,
		"status_code":   statusCode,
		"gross_amount":  grossAmount,
		"signature_key": hex.EncodeToString(sum[:]),
	}
	maps.Copy(payload, extra)
	return mustJSON(payload)
}

// Notification is SignNotification with the fields a settlement needs, which is
// the shape most tests want.
func Notification(orderID, grossAmount, serverKey, transactionStatus string, extra map[string]any) []byte {
	merged := map[string]any{
		"transaction_status": transactionStatus,
		"transaction_id":     "fake-txn-" + transactionStatus,
		"transaction_time":   time.Now().Format("2006-01-02 15:04:05"),
		"payment_type":       "bank_transfer",
		"fraud_status":       "accept",
	}
	maps.Copy(merged, extra)
	return SignNotification(orderID, "200", grossAmount, serverKey, merged)
}

// mustJSON marshals a fixture payload. A failure here is a bug in the test, not
// in the code under test, so it panics rather than threading an error through
// every caller.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("testsupport: marshalling fixture: " + err.Error())
	}
	return b
}
