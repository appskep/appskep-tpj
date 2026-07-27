package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Jadwal is the admin penjadwalan module.
//
// It follows the conventions the layanan module settled in Phase 4 — validation
// errors re-render the form at 422 with the submitted strings, toggles post the
// value they want and answer with a Turbo Stream, and every partial receives a
// struct built here rather than assembled in the template.
type Jadwal struct {
	deps *app.Deps
}

func NewJadwal(deps *app.Deps) *Jadwal {
	return &Jadwal{deps: deps}
}

// jadwalPath is where every completed action returns to.
const jadwalPath = "/admin/jadwal"

// The two views of the schedule. The calendar answers "what does this month look
// like", the list answers "what exactly is on these days" — different questions,
// so neither replaces the other.
const (
	viewCalendar = "kalender"
	viewList     = "daftar"
)

// defaultListDays is how far the list reaches when no range is given: two weeks,
// which is the window the generator's own default fills.
const defaultListDays = 13

// jadwalData is the schedule page payload.
type jadwalData struct {
	View   string
	Month  service.MonthView
	Days   []dayRows
	Filter jadwalFilter
	// Dialogs parallels the rows: one delete confirmation each, rendered outside
	// the table so the backdrop covers the page rather than a cell.
	Dialogs []confirmDialog
	// BulkConfirm is set only while a range delete is awaiting confirmation.
	BulkConfirm *bulkConfirm
	Total       int
	Clamped     bool
	Empty       emptyState
	Errors      map[string]string
	Weekdays    []string
}

// jadwalFilter is the submitted filter state, kept as strings so the form
// re-renders exactly what was asked for.
type jadwalFilter struct {
	From    string
	To      string
	Status  string
	Month   string
	Options []filterOption
}

// filterOption is one entry of the status <select>.
type filterOption struct {
	Value    string
	Label    string
	Selected bool
}

// dayRows is one date's slots under a single heading.
type dayRows struct {
	Date time.Time
	Rows []slotRow
}

// slotToggle pairs a slot with the CSRF token its inline toggle form needs. See
// serviceToggle in layanan.go for why the token has to travel in the payload.
type slotToggle struct {
	sqlc.ScheduleSlot
	CSRF string
}

// slotRow is one slot plus the three things the template would otherwise have to
// derive with comparisons it cannot express.
type slotRow struct {
	Slot     slotToggle
	Past     bool
	Full     bool
	Locked   bool
	DialogID string
}

// bulkConfirm is the second step of a range delete: the real numbers, and a form
// that re-posts the same range with konfirmasi=1.
type bulkConfirm struct {
	From      string
	To        string
	Deletable int64
	Booked    int64
}

// slotFormData drives both the create and the edit page.
type slotFormData struct {
	IsNew  bool
	Action string
	Slot   sqlc.ScheduleSlot
	// Locked marks a slot whose date and times may no longer move because it
	// already holds bookings.
	Locked      bool
	MinCapacity int32
	Form        slotFormValues
	Errors      map[string]string
}

// slotFormValues holds the submitted strings rather than parsed values, so a
// rejected submit re-renders what was typed.
type slotFormValues struct {
	Date      string
	StartTime string
	EndTime   string
	Capacity  string
	Note      string
	IsActive  bool
}

// generateData is the generator page payload.
type generateData struct {
	Form     service.GenerateInput
	Weekdays []weekdayOption
	// Plan is nil until a preview has been asked for. A non-nil plan is what the
	// page renders the confirm step from.
	Plan   *generatePlan
	Errors map[string]string
}

// weekdayOption is one weekday checkbox, ordered Monday-first for reading while
// carrying the time.Weekday number as its value.
type weekdayOption struct {
	Value   string
	Label   string
	Checked bool
}

// generatePlan is the preview, grouped by date.
//
// A flat list of up to 500 candidates tells an admin nothing; the same slots
// grouped under their date, with the days beyond the first month summarised, is
// something they can actually check before committing.
type generatePlan struct {
	NewCount      int
	ExistingCount int
	Days          int
	PerDay        int
	Shown         []planDay
	More          int
	Form          service.GenerateInput
	WeekdayValues []string
}

type planDay struct {
	Date  time.Time
	Slots []planSlot
}

type planSlot struct {
	Start  string
	End    string
	Exists bool
}

// maxPreviewDays bounds what the preview lists. Everything beyond it is reported
// as a count — the pattern repeats, so showing all 180 days adds length, not
// information.
const maxPreviewDays = 31

// Index renders the calendar or the list.
func (h *Jadwal) Index(w http.ResponseWriter, r *http.Request) {
	h.renderIndex(w, r, http.StatusOK, nil, nil)
}

// renderIndex builds the page, optionally carrying field errors or a pending
// bulk confirmation. Both of those re-render the same page rather than a
// separate one, so the admin never loses sight of the schedule they are acting
// on.
func (h *Jadwal) renderIndex(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	errs map[string]string,
	confirm *bulkConfirm,
) {
	q := r.URL.Query()

	which := q.Get("view")
	if which != viewList {
		which = viewCalendar
	}

	filter := h.filterFrom(q)
	data := jadwalData{
		View:   which,
		Filter: filter,
		Errors: errs,
	}
	if data.Errors == nil {
		data.Errors = map[string]string{}
	}
	data.BulkConfirm = confirm

	// A pending confirmation is about a range, and the bulk panel — the only place
	// a bulk message can appear — lives in the list. Rendering either into the
	// calendar would answer with a 422 the admin cannot see.
	if confirm != nil || len(data.Errors) > 0 {
		data.View = viewList
	}

	if data.View == viewCalendar {
		month, err := h.deps.Schedule.Month(r.Context(), h.deps.Schedule.ParseMonth(filter.Month))
		if err != nil {
			h.deps.Log.ErrorContext(r.Context(), "jadwal: building month", slog.Any("error", err))
			h.deps.ErrorPage(w, r, http.StatusInternalServerError)
			return
		}
		data.Month = month
		data.Total = month.Total
	} else {
		from := h.deps.Schedule.ParseDate(filter.From)
		to := h.deps.Schedule.ParseDate(filter.To)

		result, err := h.deps.Schedule.ListRange(r.Context(), service.RangeQuery{
			From:   from,
			To:     to,
			Status: filter.Status,
		})
		if err != nil {
			h.deps.Log.ErrorContext(r.Context(), "jadwal: listing range", slog.Any("error", err))
			h.deps.ErrorPage(w, r, http.StatusInternalServerError)
			return
		}

		token := auth.MaskedTokenFrom(r.Context())
		data.Days = toDayRows(result.Days, time.Now(), token)
		data.Dialogs = slotDialogs(result.Days, token)
		data.Total = result.Total
		data.Clamped = result.Clamped
		// Clamping rewrites the window, so the form must show the range actually
		// rendered rather than the one that was asked for.
		data.Filter.From = result.From.Format("2006-01-02")
		data.Filter.To = result.To.Format("2006-01-02")
	}

	data.Empty = jadwalEmptyState(data.View, filter.Status)

	h.deps.View.Render(w, r, status, "admin/jadwal", &view.View{
		Page: view.Page{Title: "Penjadwalan"},
		Data: data,
	})
}

// filterFrom reads the filter state out of the query string, defaulting the list
// window to the next two weeks.
func (h *Jadwal) filterFrom(q url.Values) jadwalFilter {
	today := h.deps.Schedule.Today()

	from := strings.TrimSpace(q.Get("dari"))
	if h.deps.Schedule.ParseDate(from).IsZero() {
		from = today.Format("2006-01-02")
	}
	to := strings.TrimSpace(q.Get("sampai"))
	if h.deps.Schedule.ParseDate(to).IsZero() {
		to = today.AddDate(0, 0, defaultListDays).Format("2006-01-02")
	}

	status := q.Get("status")
	switch status {
	case service.StatusActive, service.StatusInactive, service.StatusFull, service.StatusPast:
	default:
		status = service.StatusAll
	}

	return jadwalFilter{
		From:    from,
		To:      to,
		Status:  status,
		Month:   strings.TrimSpace(q.Get("bulan")),
		Options: statusOptions(status),
	}
}

func statusOptions(selected string) []filterOption {
	opts := []filterOption{
		{Value: service.StatusAll, Label: "Semua status"},
		{Value: service.StatusActive, Label: "Aktif"},
		{Value: service.StatusInactive, Label: "Nonaktif"},
		{Value: service.StatusFull, Label: "Penuh"},
		{Value: service.StatusPast, Label: "Sudah lewat"},
	}
	for i := range opts {
		opts[i].Selected = opts[i].Value == selected
	}
	return opts
}

// toDayRows decorates each slot with the state the template cannot compute.
func toDayRows(days []service.DayGroup, now time.Time, token string) []dayRows {
	out := make([]dayRows, 0, len(days))
	for _, d := range days {
		rows := make([]slotRow, 0, len(d.Slots))
		for _, slot := range d.Slots {
			rows = append(rows, slotRow{
				Slot:     slotToggle{ScheduleSlot: slot, CSRF: token},
				Past:     slot.StartsAt.Before(now),
				Full:     slot.BookedCount >= slot.Capacity,
				Locked:   service.Locked(slot),
				DialogID: "hapus-slot-" + strconv.FormatInt(slot.ID, 10),
			})
		}
		out = append(out, dayRows{Date: d.Date, Rows: rows})
	}
	return out
}

// slotDialogs builds one delete confirmation per slot, with the id the row's
// trigger button opens.
func slotDialogs(days []service.DayGroup, token string) []confirmDialog {
	var dialogs []confirmDialog
	for _, d := range days {
		for _, slot := range d.Slots {
			id := strconv.FormatInt(slot.ID, 10)
			body := "Slot ini akan dihapus permanen."
			if slot.BookedCount > 0 {
				body = "Slot ini sudah punya booking, jadi tidak bisa dihapus. " +
					"Nonaktifkan saja agar tidak bisa dipesan lagi."
			}
			dialogs = append(dialogs, confirmDialog{
				CSRF:         token,
				ID:           "hapus-slot-" + id,
				Title:        "Hapus slot ini?",
				Body:         body,
				ConfirmLabel: "Hapus",
				Action:       jadwalPath + "/" + id + "/hapus",
			})
		}
	}
	return dialogs
}

// jadwalEmptyState distinguishes "nothing scheduled yet" from "nothing matched
// this filter": the first wants the generator, the second wants the filter
// cleared.
func jadwalEmptyState(which, status string) emptyState {
	if status != service.StatusAll {
		return emptyState{
			Icon:        "filter",
			Title:       "Tidak ada slot yang cocok",
			Body:        "Coba ubah rentang tanggal atau tampilkan semua status.",
			ActionLabel: "Tampilkan semua",
			ActionHref:  jadwalPath + "?view=" + which,
		}
	}
	return emptyState{
		Icon:        "calendar-days",
		Title:       "Belum ada jadwal",
		Body:        "Buat slot satu per satu, atau isi sekaligus beberapa minggu dengan generator.",
		ActionLabel: "Generate jadwal",
		ActionHref:  jadwalPath + "/generate",
	}
}

// New renders the blank create form.
func (h *Jadwal) New(w http.ResponseWriter, r *http.Request) {
	// A calendar day links here with its own date, so adding a slot to a day the
	// admin is looking at does not mean retyping the date they just clicked.
	date := strings.TrimSpace(r.URL.Query().Get("tanggal"))
	if h.deps.Schedule.ParseDate(date).IsZero() {
		date = h.deps.Schedule.Today().Format("2006-01-02")
	}

	duration := h.deps.Settings.Int(service.KeySlotDefaultDuration, 150)
	capacity := h.deps.Settings.Int(service.KeySlotDefaultCapacity, 1)

	h.renderForm(w, r, http.StatusOK, slotFormData{
		IsNew:       true,
		Action:      jadwalPath,
		MinCapacity: service.MinCapacity,
		Form: slotFormValues{
			Date:      date,
			StartTime: "08:00",
			EndTime:   defaultEndTime(duration),
			Capacity:  strconv.Itoa(capacity),
			IsActive:  true,
		},
	})
}

// defaultEndTime offers a window one default slot long, so the common case is
// already filled in.
func defaultEndTime(duration int) string {
	const openMinutes = 8 * 60
	end := openMinutes + duration
	if end >= 24*60 {
		end = 24*60 - 1
	}
	return strconv.Itoa(end/60) + ":" + pad2(end%60)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// Create validates and inserts one slot.
func (h *Jadwal) Create(w http.ResponseWriter, r *http.Request) {
	in, values, ok := h.parseSlotForm(w, r)
	if !ok {
		return
	}

	if _, err := h.deps.Schedule.Create(r.Context(), in); err != nil {
		h.formError(w, r, err, slotFormData{
			IsNew:       true,
			Action:      jadwalPath,
			MinCapacity: service.MinCapacity,
			Form:        values,
		}, "jadwal: creating slot")
		return
	}

	h.deps.FlashRedirect(w, r, h.backToList(in.Date), model.FlashSuccess("Slot berhasil ditambahkan."))
}

// Edit renders the form for an existing slot.
func (h *Jadwal) Edit(w http.ResponseWriter, r *http.Request) {
	slot, ok := h.load(w, r)
	if !ok {
		return
	}

	h.renderForm(w, r, http.StatusOK, slotFormData{
		Action:      jadwalPath + "/" + strconv.FormatInt(slot.ID, 10),
		Slot:        slot,
		Locked:      service.Locked(slot),
		MinCapacity: minCapacityFor(slot),
		Form: slotFormValues{
			Date:      slot.SlotDate.Format("2006-01-02"),
			StartTime: clockValue(slot.StartTime),
			EndTime:   clockValue(slot.EndTime),
			Capacity:  strconv.Itoa(int(slot.Capacity)),
			Note:      slot.Note.String,
			IsActive:  slot.IsActive,
		},
	})
}

// Update validates and saves an existing slot.
func (h *Jadwal) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	in, values, ok := h.parseSlotForm(w, r)
	if !ok {
		return
	}

	if err := h.deps.Schedule.Update(r.Context(), id, in); err != nil {
		// Reload so a rejected save still shows the lock state and the real
		// booking count beside the submitted values.
		slot, _ := h.deps.Schedule.Get(r.Context(), id)
		h.formError(w, r, err, slotFormData{
			Action:      jadwalPath + "/" + strconv.FormatInt(id, 10),
			Slot:        slot,
			Locked:      service.Locked(slot),
			MinCapacity: minCapacityFor(slot),
			Form:        values,
		}, "jadwal: updating slot")
		return
	}

	h.deps.FlashRedirect(w, r, h.backToList(in.Date), model.FlashSuccess("Slot berhasil disimpan."))
}

// Delete removes a slot, or explains why it cannot.
func (h *Jadwal) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	switch err := h.deps.Schedule.Delete(r.Context(), id); {
	case err == nil:
		h.deps.FlashRedirect(w, r, h.referrerList(r), model.FlashSuccess("Slot berhasil dihapus."))
	case errors.Is(err, service.ErrHasBookings):
		h.deps.FlashRedirect(w, r, h.referrerList(r), model.FlashWarning(
			"Slot tidak bisa dihapus karena sudah punya booking. Nonaktifkan saja agar tidak bisa dipesan lagi."))
	case errors.Is(err, service.ErrNotFound):
		h.deps.ErrorPage(w, r, http.StatusNotFound)
	default:
		h.deps.Log.ErrorContext(r.Context(), "jadwal: deleting slot", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
	}
}

// ToggleActive flips is_active from the list.
//
// The form posts the value it wants rather than a flip, so a double-clicked
// button or a resent request settles on the state of the last click.
func (h *Jadwal) ToggleActive(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	slot, err := h.deps.Schedule.SetActive(r.Context(), id, r.PostFormValue("value") == "1")
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return
		}
		h.deps.Log.ErrorContext(r.Context(), "jadwal: toggling slot", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	if strings.Contains(r.Header.Get("Accept"), "text/vnd.turbo-stream.html") {
		h.deps.View.RenderStream(w, r, "admin/jadwal", "slot_toggle_stream", &view.View{
			Data: slotToggle{ScheduleSlot: slot, CSRF: auth.MaskedTokenFrom(r.Context())},
		})
		return
	}
	h.deps.FlashRedirect(w, r, h.referrerList(r), model.FlashSuccess("Status slot diperbarui."))
}

// Bulk applies an action to a whole date range.
//
// Activating and deactivating apply immediately: both are reversible and neither
// touches a booking. Deleting goes through a confirmation step that reports the
// real counts first, because it is the one action here that cannot be undone.
func (h *Jadwal) Bulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.deps.ErrorPage(w, r, http.StatusBadRequest)
		return
	}

	fromRaw := r.PostFormValue("bulk_from")
	toRaw := r.PostFormValue("bulk_to")
	from := h.deps.Schedule.ParseDate(fromRaw)
	to := h.deps.Schedule.ParseDate(toRaw)

	var (
		res service.BulkResult
		err error
		msg string
	)

	switch action := r.PostFormValue("aksi"); action {
	case "aktifkan", "nonaktifkan":
		res, err = h.deps.Schedule.SetActiveInRange(r.Context(), from, to, action == "aktifkan")
		if err == nil {
			verb := "diaktifkan"
			if action == "nonaktifkan" {
				verb = "dinonaktifkan"
			}
			msg = strconv.FormatInt(res.Affected, 10) + " slot " + verb + "."
		}

	case "hapus":
		if r.PostFormValue("konfirmasi") != "1" {
			preview, perr := h.deps.Schedule.PreviewDeleteInRange(r.Context(), from, to)
			if perr != nil {
				h.bulkError(w, r, perr)
				return
			}
			h.renderIndex(w, r, http.StatusOK, nil, &bulkConfirm{
				From:      fromRaw,
				To:        toRaw,
				Deletable: preview.Affected,
				Booked:    preview.Skipped,
			})
			return
		}

		res, err = h.deps.Schedule.DeleteInRange(r.Context(), from, to)
		if err == nil {
			msg = strconv.FormatInt(res.Affected, 10) + " slot dihapus."
			if res.Skipped > 0 {
				msg += " " + strconv.FormatInt(res.Skipped, 10) +
					" slot dilewati karena sudah punya booking."
			}
		}

	default:
		h.bulkError(w, r, nil)
		return
	}

	if err != nil {
		h.bulkError(w, r, err)
		return
	}

	h.deps.FlashRedirect(w, r, h.listURL(fromRaw, toRaw), model.FlashSuccess(msg))
}

// bulkError re-renders the schedule with the range messages beside their inputs.
// A nil err means the action itself was missing or unrecognised, which only a
// hand-edited form produces.
func (h *Jadwal) bulkError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *service.ValidationError
	if errors.As(err, &ve) {
		h.renderIndex(w, r, http.StatusUnprocessableEntity, ve.Fields, nil)
		return
	}
	if err == nil {
		h.renderIndex(w, r, http.StatusUnprocessableEntity,
			map[string]string{"bulk_from": "Pilih aksi massal yang valid."}, nil)
		return
	}

	h.deps.Log.ErrorContext(r.Context(), "jadwal: bulk action", slog.Any("error", err))
	h.deps.ErrorPage(w, r, http.StatusInternalServerError)
}

// GenerateForm renders the generator, prefilled from the settings table.
func (h *Jadwal) GenerateForm(w http.ResponseWriter, r *http.Request) {
	h.renderGenerate(w, r, http.StatusOK, generateData{
		Form: h.deps.Schedule.GenerateDefaults(),
	})
}

// Generate previews or applies a generator run.
//
// One route for both steps, told apart by konfirmasi: a preview is a POST
// because the form is long enough that a GET would not carry it comfortably, and
// making the two share a path is what guarantees the preview and the commit read
// the same input.
func (h *Jadwal) Generate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.deps.ErrorPage(w, r, http.StatusBadRequest)
		return
	}

	in := service.GenerateInput{
		From:        r.PostFormValue("from"),
		To:          r.PostFormValue("to"),
		WindowStart: r.PostFormValue("window_start"),
		WindowEnd:   r.PostFormValue("window_end"),
		Duration:    r.PostFormValue("duration"),
		Break:       r.PostFormValue("break"),
		Capacity:    r.PostFormValue("capacity"),
		Weekdays:    r.PostForm["weekdays"],
		IsActive:    r.PostFormValue("is_active") == "1",
	}

	if r.PostFormValue("konfirmasi") == "1" {
		res, err := h.deps.Schedule.Apply(r.Context(), in)
		if err != nil {
			h.generateError(w, r, err, in, "jadwal: applying generator")
			return
		}

		msg := strconv.Itoa(res.Created) + " slot dibuat."
		if res.Skipped > 0 {
			msg += " " + strconv.Itoa(res.Skipped) + " dilewati karena sudah ada."
		}
		h.deps.FlashRedirect(w, r, h.listURL(in.From, in.To), model.FlashSuccess(msg))
		return
	}

	plan, err := h.deps.Schedule.Plan(r.Context(), in)
	if err != nil {
		h.generateError(w, r, err, in, "jadwal: planning generator")
		return
	}

	h.renderGenerate(w, r, http.StatusOK, generateData{
		Form: in,
		Plan: toGeneratePlan(plan, in),
	})
}

// toGeneratePlan groups the candidates by date for display and carries the form
// forward so the confirm button can re-post it unchanged.
func toGeneratePlan(plan service.GeneratePlan, in service.GenerateInput) *generatePlan {
	out := &generatePlan{
		NewCount:      plan.NewCount,
		ExistingCount: plan.ExistingCount,
		Days:          plan.Days,
		PerDay:        plan.PerDay,
		Form:          in,
		WeekdayValues: in.Weekdays,
	}

	for _, c := range plan.Candidates {
		n := len(out.Shown)
		if n == 0 || !out.Shown[n-1].Date.Equal(c.Date) {
			if n >= maxPreviewDays {
				out.More++
				continue
			}
			out.Shown = append(out.Shown, planDay{Date: c.Date})
			n++
		}
		out.Shown[n-1].Slots = append(out.Shown[n-1].Slots, planSlot{
			Start:  c.Start,
			End:    c.End,
			Exists: c.Exists,
		})
	}

	return out
}

// generateError re-renders the generator with its messages, or reports a real
// failure.
func (h *Jadwal) generateError(w http.ResponseWriter, r *http.Request, err error, in service.GenerateInput, logMsg string) {
	var ve *service.ValidationError
	if errors.As(err, &ve) {
		h.renderGenerate(w, r, http.StatusUnprocessableEntity, generateData{
			Form:   in,
			Errors: ve.Fields,
		})
		return
	}

	h.deps.Log.ErrorContext(r.Context(), logMsg, slog.Any("error", err))
	h.deps.ErrorPage(w, r, http.StatusInternalServerError)
}

func (h *Jadwal) renderGenerate(w http.ResponseWriter, r *http.Request, status int, data generateData) {
	if data.Errors == nil {
		data.Errors = map[string]string{}
	}
	data.Weekdays = weekdayOptions(data.Form.Weekdays)

	h.deps.View.Render(w, r, status, "admin/jadwal-generate", &view.View{
		Page: view.Page{Title: "Generate Jadwal"},
		Data: data,
	})
}

// weekdayOptions lists the checkboxes Monday-first, which is how a working week
// reads, while keeping time.Weekday numbers as the values so the service needs
// no mapping.
func weekdayOptions(checked []string) []weekdayOption {
	set := make(map[string]bool, len(checked))
	for _, v := range checked {
		set[strings.TrimSpace(v)] = true
	}

	labels := []struct{ value, label string }{
		{"1", "Senin"}, {"2", "Selasa"}, {"3", "Rabu"}, {"4", "Kamis"},
		{"5", "Jumat"}, {"6", "Sabtu"}, {"0", "Minggu"},
	}

	opts := make([]weekdayOption, 0, len(labels))
	for _, l := range labels {
		opts = append(opts, weekdayOption{Value: l.value, Label: l.label, Checked: set[l.value]})
	}
	return opts
}

// parseSlotForm reads a slot submission. Plain form encoding, not multipart:
// there is no upload here, so none of the layanan module's body-bounding applies.
func (h *Jadwal) parseSlotForm(w http.ResponseWriter, r *http.Request) (service.SlotInput, slotFormValues, bool) {
	if err := r.ParseForm(); err != nil {
		h.deps.ErrorPage(w, r, http.StatusBadRequest)
		return service.SlotInput{}, slotFormValues{}, false
	}

	values := slotFormValues{
		Date:      r.PostFormValue("date"),
		StartTime: r.PostFormValue("start_time"),
		EndTime:   r.PostFormValue("end_time"),
		Capacity:  r.PostFormValue("capacity"),
		Note:      r.PostFormValue("note"),
		IsActive:  r.PostFormValue("is_active") == "1",
	}

	return service.SlotInput{
		Date:      values.Date,
		StartTime: values.StartTime,
		EndTime:   values.EndTime,
		Capacity:  values.Capacity,
		Note:      values.Note,
		IsActive:  values.IsActive,
	}, values, true
}

// formError renders a failed save: a validation problem re-renders the form at
// 422 with the messages, anything else is a 500.
func (h *Jadwal) formError(w http.ResponseWriter, r *http.Request, err error, data slotFormData, logMsg string) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		data.Errors = ve.Fields
		h.renderForm(w, r, http.StatusUnprocessableEntity, data)
	case errors.Is(err, service.ErrNotFound):
		h.deps.ErrorPage(w, r, http.StatusNotFound)
	default:
		h.deps.Log.ErrorContext(r.Context(), logMsg, slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
	}
}

func (h *Jadwal) renderForm(w http.ResponseWriter, r *http.Request, status int, data slotFormData) {
	title := "Ubah Slot"
	if data.IsNew {
		title = "Tambah Slot"
	}
	if data.Errors == nil {
		data.Errors = map[string]string{}
	}
	if data.MinCapacity < service.MinCapacity {
		data.MinCapacity = service.MinCapacity
	}

	h.deps.View.Render(w, r, status, "admin/jadwal-form", &view.View{
		Page: view.Page{Title: title},
		Data: data,
	})
}

// load fetches the slot named by the URL, writing the error page itself when
// there is not one.
func (h *Jadwal) load(w http.ResponseWriter, r *http.Request) (sqlc.ScheduleSlot, bool) {
	id, ok := h.idParam(w, r)
	if !ok {
		return sqlc.ScheduleSlot{}, false
	}

	slot, err := h.deps.Schedule.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return sqlc.ScheduleSlot{}, false
		}
		h.deps.Log.ErrorContext(r.Context(), "jadwal: loading slot", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return sqlc.ScheduleSlot{}, false
	}
	return slot, true
}

func (h *Jadwal) idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return 0, false
	}
	return id, true
}

// backToList returns to the list showing the day just edited, so a saved slot is
// visible rather than somewhere behind a default filter.
func (h *Jadwal) backToList(date string) string {
	if h.deps.Schedule.ParseDate(date).IsZero() {
		return jadwalPath
	}
	return h.listURL(date, date)
}

// listURL builds a list view over a date range.
func (h *Jadwal) listURL(from, to string) string {
	if h.deps.Schedule.ParseDate(from).IsZero() {
		return jadwalPath
	}
	if h.deps.Schedule.ParseDate(to).IsZero() {
		to = from
	}
	return jadwalPath + "?view=" + viewList +
		"&dari=" + url.QueryEscape(from) +
		"&sampai=" + url.QueryEscape(to)
}

// referrerList keeps a row action on the view it was invoked from.
//
// Only the query string is taken, and only from a Referer that points at this
// page: it is attacker-influenced, so it can decide a filter but must never
// decide a destination.
func (h *Jadwal) referrerList(r *http.Request) string {
	ref, err := url.Parse(r.Referer())
	if err != nil || ref.Path != jadwalPath || ref.RawQuery == "" {
		return jadwalPath
	}
	return jadwalPath + "?" + ref.RawQuery
}

// minCapacityFor is the floor the capacity input may not go below: a slot cannot
// hold fewer places than it has already promised.
func minCapacityFor(slot sqlc.ScheduleSlot) int32 {
	if slot.BookedCount > service.MinCapacity {
		return slot.BookedCount
	}
	return service.MinCapacity
}

// clockValue trims a MySQL TIME to the "HH:MM" an <input type="time"> expects;
// "08:00:00" would render as an empty input.
func clockValue(t string) string {
	if len(t) >= 5 {
		return t[:5]
	}
	return t
}
