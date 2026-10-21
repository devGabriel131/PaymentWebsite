package students

import (
	"strings"
	"unicode/utf8"
)

type Field struct {
	Key, Label, InputType string
	Required              bool
	Value                 func(Student) string
	Parse                 func(string) (any, error)
}

var fields = []Field{{
	Key: "name", Label: "Name", InputType: "text", Required: true,
	Value: func(s Student) string { return s.Name },
	Parse: func(raw string) (any, error) { return ParseName(raw, true) },
}}

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

// ParseName validates both final names and optional autosaved drafts.
func ParseName(raw string, required bool) (string, error) {
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, '\x00') {
		return "", &ValidationError{Message: "Enter a valid name."}
	}
	name := strings.TrimSpace(raw)
	if required && name == "" {
		return "", &ValidationError{Message: "Enter a name."}
	}
	return name, nil
}

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}
