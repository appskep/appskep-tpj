package service

import (
	"strings"
	"testing"
)

// validateTherapist is a package-level function over plain strings, so it needs
// no store and belongs beside the code rather than in internal/integration.

func TestValidateTherapistRejectsBadFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    TherapistInput
		field string
	}{
		{"blank name", TherapistInput{}, "name"},
		{"whitespace name", TherapistInput{Name: "   "}, "name"},
		{
			"name too long",
			TherapistInput{Name: strings.Repeat("a", maxNameLen+1)},
			"name",
		},
		{
			"specialization too long",
			TherapistInput{Name: "Budi", Specialization: strings.Repeat("b", maxNameLen+1)},
			"specialization",
		},
		{
			"experience not a number",
			TherapistInput{Name: "Budi", YearsExperience: "lima"},
			"years_experience",
		},
		{
			"experience negative",
			TherapistInput{Name: "Budi", YearsExperience: "-1"},
			"years_experience",
		},
		{
			"experience above the cap",
			TherapistInput{Name: "Budi", YearsExperience: "71"},
			"years_experience",
		},
		{
			"sort order not a number",
			TherapistInput{Name: "Budi", SortOrder: "pertama"},
			"sort_order",
		},
		{
			"sort order negative",
			TherapistInput{Name: "Budi", SortOrder: "-3"},
			"sort_order",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, ve := validateTherapist(tc.in)
			if ve == nil {
				t.Fatalf("expected a validation error on %q, got none", tc.field)
			}
			if _, ok := ve.Fields[tc.field]; !ok {
				t.Fatalf("expected an error keyed %q, got %v", tc.field, ve.Fields)
			}
		})
	}
}

// The keys must be the HTML input names, or the form renders a message with
// nowhere to sit. This is the same rule the layanan form settled in Phase 4.
func TestValidateTherapistReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	_, ve := validateTherapist(TherapistInput{
		Name:            "",
		Specialization:  strings.Repeat("x", maxNameLen+1),
		YearsExperience: "banyak",
		SortOrder:       "-1",
	})
	if ve == nil {
		t.Fatal("expected a validation error, got none")
	}

	for _, field := range []string{"name", "specialization", "years_experience", "sort_order"} {
		if _, ok := ve.Fields[field]; !ok {
			t.Errorf("expected %q among the reported fields, got %v", field, ve.Fields)
		}
	}
}

func TestValidateTherapistAcceptsAMinimalProfile(t *testing.T) {
	t.Parallel()

	p, ve := validateTherapist(TherapistInput{Name: "  Budi Santoso  "})
	if ve != nil {
		t.Fatalf("expected no validation error, got %v", ve.Fields)
	}
	if p.name != "Budi Santoso" {
		t.Errorf("name = %q, want it trimmed to %q", p.name, "Budi Santoso")
	}
	// Blank is not zero: "belum diisi" and "baru mulai" are different things to
	// show on a profile, which is why the column is nullable.
	if p.years.Valid {
		t.Errorf("years = %+v, want Valid false for an empty field", p.years)
	}
	if p.specialization.Valid || p.bio.Valid || p.certifications.Valid {
		t.Errorf("empty optional fields should stay NULL, got %+v", p)
	}
}

func TestValidateTherapistAcceptsZeroYears(t *testing.T) {
	t.Parallel()

	// The distinction the nullable column exists for: "0" is a stated answer and
	// must round-trip as one, not collapse into the empty case above.
	p, ve := validateTherapist(TherapistInput{Name: "Budi", YearsExperience: "0"})
	if ve != nil {
		t.Fatalf("expected no validation error, got %v", ve.Fields)
	}
	if !p.years.Valid || p.years.Int32 != 0 {
		t.Errorf("years = %+v, want a valid 0", p.years)
	}
}

func TestValidateTherapistAcceptsTheBounds(t *testing.T) {
	t.Parallel()

	for _, years := range []string{"0", "70"} {
		if _, ve := validateTherapist(TherapistInput{Name: "Budi", YearsExperience: years}); ve != nil {
			t.Errorf("years %q should be accepted, got %v", years, ve.Fields)
		}
	}
}
