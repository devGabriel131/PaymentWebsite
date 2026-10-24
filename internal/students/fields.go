package students

import (
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nyaruka/phonenumbers/v2"
)

var puertoRico = time.FixedZone("America/Puerto_Rico", -4*60*60)

type Field struct {
	Key, Label, InputType string
	Value                 func(Details) string
	Parse                 func(raw string, required bool) (string, error)
	Options               []string
}

var fields = []Field{
	{
		Key: "name", Label: "Name", InputType: "text",
		Value: func(d Details) string { return d.Name },
		Parse: ParseName,
	},
	{
		Key: "email", Label: "Email", InputType: "email",
		Value: func(d Details) string { return d.Email },
		Parse: ParseEmail,
	},
	{
		Key: "phone", Label: "Phone", InputType: "tel",
		Value: func(d Details) string { return d.Phone },
		Parse: ParsePhone,
	},
	{
		Key: "municipality", Label: "Municipality",
		Value: func(d Details) string { return d.Municipality },
		Parse: ParseMunicipality, Options: MunicipalityOptions(),
	},
	{
		Key: "date_of_birth", Label: "Date of birth", InputType: "date",
		Value: func(d Details) string { return d.DateOfBirth },
		Parse: ParseDateOfBirth,
	},
}

// Fields returns the ordered registry. Callers must not modify the slice.
func Fields() []Field {
	return fields
}

func FindField(key string) (Field, bool) {
	for _, field := range fields {
		if field.Key == key {
			return field, true
		}
	}
	return Field{}, false
}

// ParseName validates full names while allowing empty or one-word drafts.
func ParseName(raw string, required bool) (string, error) {
	const message = "Enter a full name using letters and spaces."
	if !utf8.ValidString(raw) {
		return "", &ValidationError{Message: message}
	}
	name := strings.TrimSpace(raw)
	if name == "" {
		if required {
			return "", &ValidationError{Message: message}
		}
		return "", nil
	}
	words, inWord := 0, false
	normalize := false
	for _, r := range name {
		switch {
		case unicode.IsLetter(r):
			if !inWord {
				words++
			}
			inWord = true
		case unicode.IsMark(r) && inWord:
		case unicode.Is(unicode.Zs, r):
			normalize = normalize || r != ' ' || !inWord
			inWord = false
		default:
			return "", &ValidationError{Message: message}
		}
	}
	if required && words < 2 {
		return "", &ValidationError{Message: message}
	}
	if !normalize {
		return name, nil
	}
	var normalized strings.Builder
	normalized.Grow(len(name))
	inWord = false
	for _, r := range name {
		if unicode.Is(unicode.Zs, r) {
			if inWord {
				normalized.WriteByte(' ')
			}
			inWord = false
			continue
		}
		normalized.WriteRune(r)
		inWord = true
	}
	return normalized.String(), nil
}

// ParseEmail accepts one bare ASCII mailbox and lowercases only its domain.
func ParseEmail(raw string, required bool) (string, error) {
	const message = "Enter a valid email address."
	email := strings.TrimSpace(raw)
	if email == "" && !required {
		return "", nil
	}
	if email == "" || len(email) > 254 || strings.ContainsRune(email, '"') {
		return "", &ValidationError{Message: message}
	}
	for i := range len(email) {
		if email[i] <= ' ' || email[i] >= 127 {
			return "", &ValidationError{Message: message}
		}
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Name != "" || address.Address != email {
		return "", &ValidationError{Message: message}
	}
	at := strings.LastIndexByte(email, '@')
	if at < 1 || at > 64 {
		return "", &ValidationError{Message: message}
	}
	domain := email[at+1:]
	if !strings.ContainsRune(domain, '.') {
		return "", &ValidationError{Message: message}
	}
	for label := range strings.SplitSeq(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", &ValidationError{Message: message}
		}
		for i := range len(label) {
			c := label[i]
			if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-') {
				return "", &ValidationError{Message: message}
			}
		}
	}
	normalizedDomain := strings.ToLower(domain)
	if normalizedDomain == domain {
		return email, nil
	}
	return email[:at+1] + normalizedDomain, nil
}

// ParsePhone accepts metadata-valid PR or US numbers and returns E.164.
func ParsePhone(raw string, required bool) (string, error) {
	const message = "Enter a valid Puerto Rico or United States phone number with area code."
	phone := strings.TrimSpace(raw)
	if phone == "" && !required {
		return "", nil
	}
	if phone == "" {
		return "", &ValidationError{Message: message}
	}
	for i := range len(phone) {
		c := phone[i]
		if !('0' <= c && c <= '9' || c == ' ' || c == '(' || c == ')' || c == '-' || c == '.' || c == '+' && i == 0) {
			return "", &ValidationError{Message: message}
		}
	}
	number, err := phonenumbers.Parse(phone, "PR")
	if err != nil {
		return "", &ValidationError{Message: message}
	}
	region := phonenumbers.GetRegionCodeForNumber(number)
	if (region != "PR" && region != "US") || !phonenumbers.IsValidNumberForRegion(number, region) {
		return "", &ValidationError{Message: message}
	}
	return phonenumbers.Format(number, phonenumbers.E164), nil
}

// ParseDateOfBirth validates a calendar date formatted as YYYY-MM-DD.
// The date must be strictly before today in Puerto Rico with age 0-100 inclusive.
func ParseDateOfBirth(raw string, required bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if required {
			return "", &ValidationError{Message: "date of birth required"}
		}
		return "", nil
	}
	t, err := time.ParseInLocation(time.DateOnly, trimmed, puertoRico)
	if err != nil {
		return "", &ValidationError{Message: "invalid date format"}
	}
	nowYear, nowMonth, nowDay := time.Now().In(puertoRico).Date()
	// Midnight today, so a birth date equal to today is not Before it.
	today := time.Date(nowYear, nowMonth, nowDay, 0, 0, 0, 0, puertoRico)
	if !t.Before(today) {
		return "", &ValidationError{Message: "date of birth must be before today"}
	}
	birthYear, birthMonth, birthDay := t.Date()
	age := nowYear - birthYear
	if nowMonth < birthMonth || (nowMonth == birthMonth && nowDay < birthDay) {
		age--
	}
	if age > 100 {
		return "", &ValidationError{Message: "age must be between 0 and 100"}
	}
	return t.Format(time.DateOnly), nil
}

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}
