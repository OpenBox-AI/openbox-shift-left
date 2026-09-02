// Package artifact implements the local project-assurance artifact primitives.
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	digestPrefix      = "sha256:"
	maxCanonicalDepth = 1024
)

var (
	contentDigestType = reflect.TypeOf(ContentDigest{})
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// ContentDigest is a SHA-256 content identifier rendered as
// sha256:<64 lowercase hexadecimal digits>.
type ContentDigest [sha256.Size]byte

// DigestBytes returns the content identifier of the exact supplied bytes.
func DigestBytes(data []byte) ContentDigest {
	return ContentDigest(sha256.Sum256(data))
}

// ParseContentDigest parses the closed project-assurance digest syntax.
func ParseContentDigest(value string) (ContentDigest, error) {
	var digest ContentDigest
	if len(value) != len(digestPrefix)+hex.EncodedLen(len(digest)) || !strings.HasPrefix(value, digestPrefix) {
		return digest, fmt.Errorf("artifact: invalid content digest %q", value)
	}
	hexPart := value[len(digestPrefix):]
	for _, character := range hexPart {
		if !('0' <= character && character <= '9') && !('a' <= character && character <= 'f') {
			return digest, fmt.Errorf("artifact: invalid content digest %q", value)
		}
	}
	if _, err := hex.Decode(digest[:], []byte(hexPart)); err != nil {
		return ContentDigest{}, fmt.Errorf("artifact: decode content digest: %w", err)
	}
	return digest, nil
}

func (digest ContentDigest) String() string {
	return digestPrefix + hex.EncodeToString(digest[:])
}

func (digest ContentDigest) MarshalText() ([]byte, error) {
	return []byte(digest.String()), nil
}

func (digest *ContentDigest) UnmarshalText(text []byte) error {
	if digest == nil {
		return errors.New("artifact: nil ContentDigest receiver")
	}
	parsed, err := ParseContentDigest(string(text))
	if err != nil {
		return err
	}
	*digest = parsed
	return nil
}

// CanonicalJSON marshals a Go value and returns RFC 8785 canonical JSON.
// Field-specific constraints, including the public schemas' signed-53-bit
// integer bounds, must be validated before canonicalization; JCS itself works
// over the full finite IEEE-754 number domain. User-defined JSON and text
// marshalers are rejected so they cannot silently replace invalid Unicode or
// make content identity depend on an opaque transformation.
func CanonicalJSON(value any) ([]byte, error) {
	if err := validateGoValue(reflect.ValueOf(value), make(map[visit]bool)); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("artifact: marshal JSON: %w", err)
	}
	return CanonicalizeJSON(raw)
}

// DigestCanonicalJSON returns both the canonical bytes and their content ID.
func DigestCanonicalJSON(value any) ([]byte, ContentDigest, error) {
	canonical, err := CanonicalJSON(value)
	if err != nil {
		return nil, ContentDigest{}, err
	}
	return canonical, DigestBytes(canonical), nil
}

// CanonicalizeJSON parses one I-JSON value and returns RFC 8785 canonical
// bytes. It rejects duplicate object names, invalid UTF-8, lone UTF-16
// surrogates, trailing data, non-finite numbers, and excessive nesting. The
// caller must validate project-schema field constraints before canonicalizing.
func CanonicalizeJSON(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("artifact: JSON is not valid UTF-8")
	}
	parser := jsonParser{raw: raw}
	value, err := parser.parseValue(0)
	if err != nil {
		return nil, err
	}
	parser.skipSpace()
	if parser.offset != len(raw) {
		return nil, parser.errorf("trailing data")
	}
	var output bytes.Buffer
	value.appendCanonical(&output)
	return output.Bytes(), nil
}

type visit struct {
	typeOf  reflect.Type
	pointer uintptr
}

func validateGoValue(value reflect.Value, visiting map[visit]bool) error {
	if !value.IsValid() {
		return nil
	}
	if hasCustomMarshaler(value.Type()) && !isContentDigest(value.Type()) {
		return fmt.Errorf("artifact: custom JSON/text marshaler %s is not supported", value.Type())
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return validateGoValue(value.Elem(), visiting)
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		key := visit{typeOf: value.Type(), pointer: value.Pointer()}
		if visiting[key] {
			return errors.New("artifact: cyclic Go value")
		}
		visiting[key] = true
		defer delete(visiting, key)
		return validateGoValue(value.Elem(), visiting)
	}

	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return errors.New("artifact: Go string is not valid UTF-8")
		}
	case reflect.Float32, reflect.Float64:
		number := value.Float()
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return errors.New("artifact: non-finite number")
		}
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		key := visit{typeOf: value.Type(), pointer: value.Pointer()}
		if visiting[key] {
			return errors.New("artifact: cyclic Go value")
		}
		visiting[key] = true
		defer delete(visiting, key)
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateGoValue(iterator.Key(), visiting); err != nil {
				return err
			}
			if err := validateGoValue(iterator.Value(), visiting); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if value.IsNil() {
			return nil
		}
		key := visit{typeOf: value.Type(), pointer: value.Pointer()}
		if visiting[key] {
			return errors.New("artifact: cyclic Go value")
		}
		visiting[key] = true
		defer delete(visiting, key)
		for index := 0; index < value.Len(); index++ {
			if err := validateGoValue(value.Index(index), visiting); err != nil {
				return err
			}
		}
	case reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if err := validateGoValue(value.Index(index), visiting); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if field.PkgPath != "" {
				continue
			}
			if err := validateGoValue(value.Field(index), visiting); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasCustomMarshaler(valueType reflect.Type) bool {
	if valueType.Implements(jsonMarshalerType) || valueType.Implements(textMarshalerType) {
		return true
	}
	return valueType.Kind() != reflect.Pointer &&
		(reflect.PointerTo(valueType).Implements(jsonMarshalerType) ||
			reflect.PointerTo(valueType).Implements(textMarshalerType))
}

func isContentDigest(valueType reflect.Type) bool {
	return valueType == contentDigestType ||
		(valueType.Kind() == reflect.Pointer && valueType.Elem() == contentDigestType)
}

type valueKind uint8

const (
	nullValue valueKind = iota
	boolValue
	numberValue
	stringValue
	arrayValue
	objectValue
)

type jsonValue struct {
	kind    valueKind
	boolean bool
	number  float64
	text    string
	array   []*jsonValue
	object  []jsonMember
}

type jsonMember struct {
	name  string
	value *jsonValue
}

func (value *jsonValue) appendCanonical(output *bytes.Buffer) {
	switch value.kind {
	case nullValue:
		output.WriteString("null")
	case boolValue:
		if value.boolean {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case numberValue:
		output.WriteString(formatNumber(value.number))
	case stringValue:
		appendJSONString(output, value.text)
	case arrayValue:
		output.WriteByte('[')
		for index, item := range value.array {
			if index > 0 {
				output.WriteByte(',')
			}
			item.appendCanonical(output)
		}
		output.WriteByte(']')
	case objectValue:
		sort.Slice(value.object, func(left, right int) bool {
			return utf16Less(value.object[left].name, value.object[right].name)
		})
		output.WriteByte('{')
		for index, member := range value.object {
			if index > 0 {
				output.WriteByte(',')
			}
			appendJSONString(output, member.name)
			output.WriteByte(':')
			member.value.appendCanonical(output)
		}
		output.WriteByte('}')
	}
}

func utf16Less(left, right string) bool {
	leftUnits := utf16.Encode([]rune(left))
	rightUnits := utf16.Encode([]rune(right))
	for index := 0; index < len(leftUnits) && index < len(rightUnits); index++ {
		if leftUnits[index] != rightUnits[index] {
			return leftUnits[index] < rightUnits[index]
		}
	}
	return len(leftUnits) < len(rightUnits)
}

func appendJSONString(output *bytes.Buffer, value string) {
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(character)
		case '\b':
			output.WriteString(`\b`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\f':
			output.WriteString(`\f`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if character < 0x20 {
				fmt.Fprintf(output, `\u%04x`, character)
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
}

func formatNumber(number float64) string {
	if number == 0 {
		return "0"
	}
	negative := math.Signbit(number)
	absolute := math.Abs(number)
	scientific := strconv.FormatFloat(absolute, 'e', -1, 64)
	parts := strings.SplitN(scientific, "e", 2)
	digits := strings.ReplaceAll(parts[0], ".", "")
	exponent, _ := strconv.Atoi(parts[1])

	var formatted string
	if absolute >= 1e-6 && absolute < 1e21 {
		position := exponent + 1
		switch {
		case position <= 0:
			formatted = "0." + strings.Repeat("0", -position) + digits
		case position >= len(digits):
			formatted = digits + strings.Repeat("0", position-len(digits))
		default:
			formatted = digits[:position] + "." + digits[position:]
		}
	} else {
		formatted = digits[:1]
		if len(digits) > 1 {
			formatted += "." + digits[1:]
		}
		formatted += "e"
		if exponent >= 0 {
			formatted += "+"
		}
		formatted += strconv.Itoa(exponent)
	}
	if negative {
		return "-" + formatted
	}
	return formatted
}

type jsonParser struct {
	raw    []byte
	offset int
}

func (parser *jsonParser) parseValue(depth int) (*jsonValue, error) {
	parser.skipSpace()
	if parser.offset >= len(parser.raw) {
		return nil, parser.errorf("unexpected end of JSON")
	}
	switch parser.raw[parser.offset] {
	case 'n':
		if !parser.consumeLiteral("null") {
			return nil, parser.errorf("invalid literal")
		}
		return &jsonValue{kind: nullValue}, nil
	case 't':
		if !parser.consumeLiteral("true") {
			return nil, parser.errorf("invalid literal")
		}
		return &jsonValue{kind: boolValue, boolean: true}, nil
	case 'f':
		if !parser.consumeLiteral("false") {
			return nil, parser.errorf("invalid literal")
		}
		return &jsonValue{kind: boolValue}, nil
	case '"':
		text, err := parser.parseString()
		if err != nil {
			return nil, err
		}
		return &jsonValue{kind: stringValue, text: text}, nil
	case '[':
		if depth >= maxCanonicalDepth {
			return nil, parser.errorf("nesting exceeds %d", maxCanonicalDepth)
		}
		return parser.parseArray(depth + 1)
	case '{':
		if depth >= maxCanonicalDepth {
			return nil, parser.errorf("nesting exceeds %d", maxCanonicalDepth)
		}
		return parser.parseObject(depth + 1)
	default:
		return parser.parseNumber()
	}
}

func (parser *jsonParser) parseArray(depth int) (*jsonValue, error) {
	parser.offset++
	array := &jsonValue{kind: arrayValue}
	parser.skipSpace()
	if parser.consumeByte(']') {
		return array, nil
	}
	for {
		item, err := parser.parseValue(depth)
		if err != nil {
			return nil, err
		}
		array.array = append(array.array, item)
		parser.skipSpace()
		if parser.consumeByte(']') {
			return array, nil
		}
		if !parser.consumeByte(',') {
			return nil, parser.errorf("expected ',' or ']'")
		}
	}
}

func (parser *jsonParser) parseObject(depth int) (*jsonValue, error) {
	parser.offset++
	object := &jsonValue{kind: objectValue}
	names := make(map[string]struct{})
	parser.skipSpace()
	if parser.consumeByte('}') {
		return object, nil
	}
	for {
		parser.skipSpace()
		if parser.offset >= len(parser.raw) || parser.raw[parser.offset] != '"' {
			return nil, parser.errorf("expected object name")
		}
		name, err := parser.parseString()
		if err != nil {
			return nil, err
		}
		if _, exists := names[name]; exists {
			return nil, parser.errorf("duplicate object name %q", name)
		}
		names[name] = struct{}{}
		parser.skipSpace()
		if !parser.consumeByte(':') {
			return nil, parser.errorf("expected ':'")
		}
		memberValue, err := parser.parseValue(depth)
		if err != nil {
			return nil, err
		}
		object.object = append(object.object, jsonMember{name: name, value: memberValue})
		parser.skipSpace()
		if parser.consumeByte('}') {
			return object, nil
		}
		if !parser.consumeByte(',') {
			return nil, parser.errorf("expected ',' or '}'")
		}
	}
}

func (parser *jsonParser) parseString() (string, error) {
	parser.offset++
	var decoded strings.Builder
	for parser.offset < len(parser.raw) {
		character := parser.raw[parser.offset]
		switch {
		case character == '"':
			parser.offset++
			return decoded.String(), nil
		case character == '\\':
			parser.offset++
			if parser.offset >= len(parser.raw) {
				return "", parser.errorf("unfinished string escape")
			}
			escaped := parser.raw[parser.offset]
			parser.offset++
			switch escaped {
			case '"', '\\', '/':
				decoded.WriteByte(escaped)
			case 'b':
				decoded.WriteByte('\b')
			case 'f':
				decoded.WriteByte('\f')
			case 'n':
				decoded.WriteByte('\n')
			case 'r':
				decoded.WriteByte('\r')
			case 't':
				decoded.WriteByte('\t')
			case 'u':
				unicodeValue, err := parser.parseUnicodeEscape()
				if err != nil {
					return "", err
				}
				decoded.WriteRune(unicodeValue)
			default:
				return "", parser.errorf("invalid string escape")
			}
		case character < 0x20:
			return "", parser.errorf("unescaped control character")
		default:
			runeValue, size := utf8.DecodeRune(parser.raw[parser.offset:])
			if runeValue == utf8.RuneError && size == 1 {
				return "", parser.errorf("invalid UTF-8 in string")
			}
			decoded.WriteRune(runeValue)
			parser.offset += size
		}
	}
	return "", parser.errorf("unterminated string")
}

func (parser *jsonParser) parseUnicodeEscape() (rune, error) {
	first, err := parser.parseHexUnit()
	if err != nil {
		return 0, err
	}
	if 0xD800 <= first && first <= 0xDBFF {
		if parser.offset+2 > len(parser.raw) || parser.raw[parser.offset] != '\\' || parser.raw[parser.offset+1] != 'u' {
			return 0, parser.errorf("high surrogate without low surrogate")
		}
		parser.offset += 2
		second, secondErr := parser.parseHexUnit()
		if secondErr != nil {
			return 0, secondErr
		}
		if second < 0xDC00 || second > 0xDFFF {
			return 0, parser.errorf("high surrogate without low surrogate")
		}
		return utf16.DecodeRune(rune(first), rune(second)), nil
	}
	if 0xDC00 <= first && first <= 0xDFFF {
		return 0, parser.errorf("low surrogate without high surrogate")
	}
	return rune(first), nil
}

func (parser *jsonParser) parseHexUnit() (uint16, error) {
	if parser.offset+4 > len(parser.raw) {
		return 0, parser.errorf("unfinished Unicode escape")
	}
	var result uint16
	for count := 0; count < 4; count++ {
		character := parser.raw[parser.offset]
		parser.offset++
		result <<= 4
		switch {
		case '0' <= character && character <= '9':
			result += uint16(character - '0')
		case 'a' <= character && character <= 'f':
			result += uint16(character-'a') + 10
		case 'A' <= character && character <= 'F':
			result += uint16(character-'A') + 10
		default:
			return 0, parser.errorf("invalid Unicode escape")
		}
	}
	return result, nil
}

func (parser *jsonParser) parseNumber() (*jsonValue, error) {
	start := parser.offset
	parser.consumeByte('-')
	if parser.offset >= len(parser.raw) {
		return nil, parser.errorf("invalid number")
	}
	if parser.consumeByte('0') {
		if parser.offset < len(parser.raw) && '0' <= parser.raw[parser.offset] && parser.raw[parser.offset] <= '9' {
			return nil, parser.errorf("leading zero in number")
		}
	} else {
		if !parser.consumeDigits(true) {
			return nil, parser.errorf("invalid number")
		}
	}
	if parser.consumeByte('.') && !parser.consumeDigits(true) {
		return nil, parser.errorf("fraction requires digits")
	}
	if parser.offset < len(parser.raw) && (parser.raw[parser.offset] == 'e' || parser.raw[parser.offset] == 'E') {
		parser.offset++
		if parser.offset < len(parser.raw) && (parser.raw[parser.offset] == '+' || parser.raw[parser.offset] == '-') {
			parser.offset++
		}
		if !parser.consumeDigits(true) {
			return nil, parser.errorf("exponent requires digits")
		}
	}
	number, err := strconv.ParseFloat(string(parser.raw[start:parser.offset]), 64)
	if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
		return nil, parser.errorf("number is outside the finite IEEE-754 domain")
	}
	return &jsonValue{kind: numberValue, number: number}, nil
}

func (parser *jsonParser) consumeDigits(require bool) bool {
	start := parser.offset
	for parser.offset < len(parser.raw) && '0' <= parser.raw[parser.offset] && parser.raw[parser.offset] <= '9' {
		parser.offset++
	}
	return !require || parser.offset > start
}

func (parser *jsonParser) consumeLiteral(literal string) bool {
	if !bytes.HasPrefix(parser.raw[parser.offset:], []byte(literal)) {
		return false
	}
	parser.offset += len(literal)
	return true
}

func (parser *jsonParser) consumeByte(character byte) bool {
	if parser.offset >= len(parser.raw) || parser.raw[parser.offset] != character {
		return false
	}
	parser.offset++
	return true
}

func (parser *jsonParser) skipSpace() {
	for parser.offset < len(parser.raw) {
		switch parser.raw[parser.offset] {
		case ' ', '\t', '\n', '\r':
			parser.offset++
		default:
			return
		}
	}
}

func (parser *jsonParser) errorf(format string, arguments ...any) error {
	return fmt.Errorf("artifact: JSON byte %d: %s", parser.offset, fmt.Sprintf(format, arguments...))
}
