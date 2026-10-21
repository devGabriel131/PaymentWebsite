package students

import (
	"strings"
	"unicode/utf8"
)

type Field interface {
	Key() string
	Label() string
	InputType() string
	Required() bool
	Value(Student) string
	Parse(string) (any, error)
}

var fields = []Field{nameField{}}

// Fields returns the ordered registry. Callers must not modify the slice.
func Fields() []Field {
	return fields
}

func FindField(key string) (Field, bool) {
	for _, field := range fields {
		if field.Key() == key {
			return field, true
		}
	}
	return nil, false
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

type nameField struct{}

func (nameField) Key() string            { return "name" }
func (nameField) Label() string          { return "Name" }
func (nameField) InputType() string      { return "text" }
func (nameField) Required() bool         { return true }
func (nameField) Value(s Student) string { return s.Name }
func (nameField) Parse(raw string) (any, error) {
	return ParseName(raw, true)
}
