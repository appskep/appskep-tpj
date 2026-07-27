package service

import "fmt"

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
