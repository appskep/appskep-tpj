package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/app/public"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The review panel is a <dialog>, and the `open` attribute on it is the whole
// no-JS path.
//
// A <dialog> without `open` is display:none until something calls showModal(),
// and nothing on this page can: script-src is 'self' with no nonce, so there is
// no inline script, and app.js's other dialog path is a click handler with no
// trigger to click. The server therefore renders it open and app.js upgrades it.
//
// Drop the attribute and the summary vanishes for anyone whose JavaScript failed
// to load — while the markup is still in the document, so every other assertion
// in this package stays green and curl reports a perfectly correct page. That is
// exactly the class of defect this file exists to catch.
func TestTheReviewPanelIsADialogRenderedOpen(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9411)
	slot := env.FutureSlot(3, 1)
	cookie, csrf := env.SignedIn(user.AppskepUserID)

	form := url.Values{
		"_csrf":   {csrf},
		"layanan": {testsupport.SlugUrut},
		"tanggal": {slot.SlotDate.Format("2006-01-02")},
		"slot":    {fmt.Sprint(slot.ID)},
		"nama":    {"Budi Santoso"},
		"telepon": {"081234567890"},
		"alamat":  {"Jl. Contoh No. 1"},
	}

	r := httptest.NewRequest(http.MethodPost, "/booking", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)

	rec := httptest.NewRecorder()
	public.Routes(env.Deps).ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("the review POST = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	start := strings.Index(body, `<dialog id="ringkasan"`)
	if start < 0 {
		t.Fatal("the review panel is not a <dialog id=\"ringkasan\">")
	}
	end := strings.Index(body[start:], ">")
	if end < 0 {
		t.Fatal("the <dialog> tag is unterminated")
	}
	tag := body[start : start+end]

	// A bare attribute, not open="true" — and asserted on the opening tag alone,
	// because the word appears all over the page otherwise.
	if !strings.Contains(tag, " open") {
		t.Errorf("the <dialog> is not rendered open, so the no-JS path sees nothing: %s", tag)
	}
	// The hook app.js upgrades on. Without it the dialog stays non-modal: visible,
	// but in flow, with no backdrop and no Escape.
	if !strings.Contains(tag, "data-dialog-open") {
		t.Errorf("the <dialog> carries no data-dialog-open, so app.js never upgrades it: %s", tag)
	}

	// The commit form still resolves inside it. confirmForm walks backwards from
	// name="konfirmasi" to the nearest preceding <form, and the dialog introduces
	// two more forms on the page — the close button and "Ubah data". Both must
	// stay outside it.
	confirm := confirmForm(t, body)
	if strings.HasPrefix(confirm, `<form method="dialog"`) {
		t.Error("confirmForm resolved to a dialog close form; one of them sits between the commit form and its konfirmasi input")
	}
	assertInputValue(t, confirm, "nama", "Budi Santoso")
	if !strings.Contains(confirm, `data-turbo-frame="_top"`) {
		t.Error("the commit form lost data-turbo-frame=\"_top\"; inside the frame Turbo empties it instead of navigating")
	}

	// Still a dry run.
	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d after a review, want 0", got)
	}

	env.AssertInvariant()
}

// A rejected submit re-renders at 422 with no dialog at all.
//
// Review is false on that path, so the summary must not appear — a modal over a
// form the customer has to correct would hide the error messages it is asking
// them to read.
func TestARejectedSubmitRendersNoDialog(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9412)
	slot := env.FutureSlot(3, 1)
	cookie, csrf := env.SignedIn(user.AppskepUserID)

	form := url.Values{
		"_csrf":   {csrf},
		"layanan": {testsupport.SlugUrut},
		"tanggal": {slot.SlotDate.Format("2006-01-02")},
		"slot":    {fmt.Sprint(slot.ID)},
		"nama":    {""}, // required
		"telepon": {"081234567890"},
		"alamat":  {"Jl. Contoh No. 1"},
	}

	r := httptest.NewRequest(http.MethodPost, "/booking", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)

	rec := httptest.NewRecorder()
	public.Routes(env.Deps).ServeHTTP(rec, r)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a rejected review POST = %d, want 422", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `<dialog id="ringkasan"`) {
		t.Error("the 422 re-render carries the ringkasan dialog, which would cover the field errors")
	}

	env.AssertInvariant()
}
