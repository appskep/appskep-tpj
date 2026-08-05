package service

import (
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The primitives every validator in this package was repeating.
//
// Six validators — catalog, booking, profile, schedule (twice) and settings —
// each wrote out the same four checks by hand: trim, required, rune-length, and
// the phone rules. Collecting them here is de-duplication only: every message is
// byte-identical to the one it replaces, because they are what the customer
// reads and Phase 4's rule is that a rejected form reports every problem in
// Bahasa Indonesia beside the input that caused it.
//
// Rune length, never len(): the fields hold Indonesian names and addresses, and
// a byte count would reject a shorter string than the one the column can hold
// and than the one the message promises.
//
// These take the field key and the label so a caller reads as one line, and they
// return whether the value passed, so a caller can stop at the first problem for
// one field — reporting "wajib diisi" and "maksimal 150 karakter" about the same
// empty box helps nobody.

// required adds "wajib diisi" when the value is empty.
func required(ve *ValidationError, field, label, value string) bool {
	if value == "" {
		ve.Add(field, label+" wajib diisi.")
		return false
	}
	return true
}

// maxLen adds the length message when the value is longer than max runes.
func maxLen(ve *ValidationError, field, label, value string, max int) bool {
	if len([]rune(value)) > max {
		ve.Add(field, fmt.Sprintf("%s maksimal %d karakter.", label, max))
		return false
	}
	return true
}

// requiredMaxLen is the pair applied in order, which is what most fields want.
func requiredMaxLen(ve *ValidationError, field, label, value string, max int) bool {
	return required(ve, field, label, value) && maxLen(ve, field, label, value, max)
}

// validPhoneShape allows the punctuation people actually type into a phone
// field, and nothing else. It deliberately does not try to parse a number: a
// pattern strict enough to be useful would reject some real number, and the
// phone is for a human to call, not for a machine to dial.
func validPhoneShape(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '+' || r == '-' || r == ' ' || r == '(' || r == ')':
		default:
			return false
		}
	}
	return true
}

func countDigits(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
}

// phone applies the shape and digit-count rules to a non-empty value.
//
// It lives here rather than in booking.go, where it grew, because the profile
// form borrows it verbatim: a number accepted while booking must not be rejected
// when saved to the profile, and the only way to guarantee that is one
// implementation. Whether the field is required at all is the caller's decision
// — it is on the booking form and optional on the profile.
func phone(ve *ValidationError, field, value string) bool {
	switch {
	case !validPhoneShape(value):
		ve.Add(field, "Nomor telepon hanya boleh berisi angka, spasi, dan tanda + - ( ).")
		return false
	case countDigits(value) < minPhoneDigits:
		ve.Add(field, fmt.Sprintf("Nomor telepon minimal %d angka.", minPhoneDigits))
		return false
	}
	return true
}

// The map pin, shared by the booking form and the profile form for the same
// reason phone is: a location accepted while booking must not be rejected when
// saved to the profile, and one implementation is the only way to guarantee it.

const (
	// coordFieldKey is the ValidationError key every coordinate message uses.
	//
	// Not "latitude"/"longitude": those are hidden inputs written by the map
	// widget, and a message keyed to one has nowhere to render. "lokasi" names
	// the visible map card, which is where the message goes — the same reasoning
	// that promotes layanan and slot errors to the page notice.
	coordFieldKey = "lokasi"

	// coordDecimals matches DECIMAL(9,7)/DECIMAL(10,7) in the schema. Formatting
	// to it here rather than letting MariaDB round means the value validated is
	// byte-for-byte the value stored, and a re-rendered form shows what will be
	// written rather than what was typed.
	coordDecimals = 7

	maxLatitude  = 90.0
	maxLongitude = 180.0
)

// coordinate parses one WGS84 degree value and returns it normalised.
//
// The map widget writes this field, never a human, so anything unparseable is a
// tampered or truncated hidden input rather than a typo — which is why all three
// failures share one message rather than explaining the distinction to someone
// who never typed it.
func coordinate(value string, limit float64) (string, bool) {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > limit {
		return "", false
	}
	return strconv.FormatFloat(f, 'f', coordDecimals, 64), true
}

// coordinatePair validates latitude and longitude together and returns them
// ready to write.
//
// Both or neither. The pin is optional everywhere it appears, so two empty
// strings are a valid answer meaning "not provided" — but half a pin is not a
// location, and letting one through would put a row in the database that no
// page can render and no therapist can drive to.
func coordinatePair(ve *ValidationError, lat, lng string) (sql.NullString, sql.NullString) {
	lat, lng = strings.TrimSpace(lat), strings.TrimSpace(lng)
	if lat == "" && lng == "" {
		return sql.NullString{}, sql.NullString{}
	}

	normLat, latOK := coordinate(lat, maxLatitude)
	normLng, lngOK := coordinate(lng, maxLongitude)
	if !latOK || !lngOK {
		ve.Add(coordFieldKey, "Titik lokasi tidak valid. Coba deteksi ulang atau geser pin di peta.")
		return sql.NullString{}, sql.NullString{}
	}

	return sql.NullString{String: normLat, Valid: true}, sql.NullString{String: normLng, Valid: true}
}
