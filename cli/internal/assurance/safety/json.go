// Package safety contains shared, closed checks for sensitive assurance data.
package safety

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

// JSONContainsCredentialMaterial parses exactly one JSON value and reports
// whether a credential-shaped key or value is present.
func JSONContainsCredentialMaterial(body []byte) (bool, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false, errors.New("cannot inspect JSON for credential material")
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return false, errors.New("JSON contains trailing data")
	}
	return credentialMaterial(reflect.ValueOf(value)), nil
}

func credentialMaterial(value reflect.Value) bool {
	if !value.IsValid() {
		return false
	}
	if value.Kind() == reflect.Interface {
		return credentialMaterial(value.Elem())
	}
	switch value.Kind() {
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if forbiddenCredentialKey(iterator.Key().String()) || credentialMaterial(iterator.Value()) {
				return true
			}
		}
	case reflect.Slice:
		for index := 0; index < value.Len(); index++ {
			if credentialMaterial(value.Index(index)) {
				return true
			}
		}
	case reflect.String:
		text := value.String()
		return strings.Contains(text, "-----BEGIN PRIVATE KEY-----") || strings.Contains(text, "obx_")
	}
	return false
}

func forbiddenCredentialKey(name string) bool {
	var normalized strings.Builder
	for index, character := range name {
		if character == '-' || character == ' ' {
			normalized.WriteByte('_')
			continue
		}
		if character >= 'A' && character <= 'Z' {
			if index > 0 {
				normalized.WriteByte('_')
			}
			normalized.WriteRune(character + ('a' - 'A'))
			continue
		}
		normalized.WriteRune(character)
	}
	key := strings.ToLower(normalized.String())
	switch key {
	case "token", "authorization", "credential", "password", "secret", "private_key", "api_key":
		return true
	}
	return strings.HasSuffix(key, "_token") || strings.HasSuffix(key, "_password") ||
		strings.HasSuffix(key, "_secret") || strings.HasSuffix(key, "_private_key") || strings.HasSuffix(key, "_api_key")
}
