package students_test

import (
	"errors"
	"testing"

	"paymentwebsite/internal/students"
)

func TestParseNamePreservesUnicodeAndDraftBoundary(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		required bool
		want     string
		message  string
	}{
		{"Unicode whitespace trimmed", "\u2003 María O'Connor 李 \t\n", true, "María O'Connor 李", ""},
		{"markup remains plain data", `<b>Ana & 李</b>`, true, `<b>Ana & 李</b>`, ""},
		{"empty draft allowed", "", false, "", ""},
		{"blank draft allowed", " \t\u2003\n", false, "", ""},
		{"empty final refused", "", true, "", "Enter a name."},
		{"blank final refused", " \t\u2003\n", true, "", "Enter a name."},
		{"invalid UTF8 final refused", "Ana\xff", true, "", "Enter a valid name."},
		{"invalid UTF8 draft refused", "\xff", false, "", "Enter a valid name."},
		{"NUL final refused", "Ana\x00", true, "", "Enter a valid name."},
		{"NUL draft refused", "\x00", false, "", "Enter a valid name."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := students.ParseName(tc.raw, tc.required)
			if got != tc.want {
				t.Fatalf("parsed %q, want %q", got, tc.want)
			}
			if tc.message == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var invalid *students.ValidationError
			if !errors.As(err, &invalid) || invalid.Message != tc.message || err.Error() != tc.message {
				t.Fatalf("error = %v, want ValidationError %q", err, tc.message)
			}
		})
	}
}
