// Command tinypbc generates Go structs, encoders and decoders for protobuf
// messages defined in a minimal proto-subset IDL (see
// internal/esphome/api.pidl for an example). The generated code uses the
// tinypb package in this module as its wire codec.
//
// Usage (via go:generate, from the package directory):
//
//	//go:generate go run ../../tinypb/cmd/tinypbc -in api.pidl -out api_gen.go
package main

import (
	"flag"
	"fmt"
	"go/format"
	"os"
	"sort"
	"strings"
)

type field struct {
	repeated bool
	optional bool
	typ      string // IDL type (scalar or message name)
	number   int
	name     string
}

type message struct {
	name       string
	typeNumber int // ESPHome API message type; 0 = nested-only
	fields     []field
}

// Scalar kinds: IDL type -> Go type, wire type, pb reader call.
var scalars = map[string]struct {
	goType string
	wire   string
	reader string
}{
	"uint32":  {"uint32", "WireVarint", "Uint32"},
	"uint64":  {"uint64", "WireVarint", "Varint"},
	"sint32":  {"int32", "WireVarint", "Sint32"},
	"bool":    {"bool", "WireVarint", "Bool"},
	"fixed32": {"uint32", "WireFixed32", "Fixed32"},
	"fixed64": {"uint64", "WireFixed64", "Fixed64"},
	"float":   {"float32", "WireFixed32", "Float32"},
	"string":  {"string", "WireBytes", "String"},
	"bytes":   {"[]byte", "WireBytes", "Bytes"},
}

var goKeywords = map[string]bool{
	"type": true, "func": true, "var": true, "return": true, "range": true, "map": true,
}

func main() {
	in := flag.String("in", "api.pidl", "IDL input file")
	out := flag.String("out", "api_gen.go", "generated Go output file")
	flag.Parse()

	msgs, err := parseFile(*in)
	if err != nil {
		fatal("parse %s: %v", *in, err)
	}
	if err := validate(msgs); err != nil {
		fatal("validate: %v", err)
	}

	src, err := generate(msgs)
	if err != nil {
		fatal("generate: %v", err)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		fatal("write %s: %v", *out, err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "tinypbc: "+format+"\n", args...)
	os.Exit(1)
}

// --- parser ---------------------------------------------------------------

type parser struct {
	toks []token
	pos  int
}

type token struct {
	kind string // "ident", "number", "punct", "eof"
	text string
}

func parseFile(path string) ([]message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := stripComments(string(data))
	p := &parser{toks: tokenize(text)}
	return p.parseMessages()
}

// stripComments removes // line comments.
func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func tokenize(s string) []token {
	var toks []token
	// Simple scanner: identifiers/numbers vs punctuation.
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')' || c == '{' || c == '}' || c == '=' || c == ';':
			toks = append(toks, token{"punct", string(c)})
			i++
		case isIdentChar(c) || c == '-':
			j := i
			for j < len(s) && (isIdentChar(s[j])) {
				j++
			}
			toks = append(toks, token{identKind(s[i:j]), s[i:j]})
			i = j
		default:
			// Unknown character: emit an invalid token so the parser
			// reports it instead of panicking on a nil token list.
			toks = append(toks, token{"invalid", string(c)})
			i++
		}
	}
	toks = append(toks, token{"eof", ""})
	return toks
}

func isIdentChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// identKind classifies a word: all-digits is a number; a word starting
// with a digit but containing non-digits ("9foo") is invalid and is
// rejected wherever it appears.
func identKind(s string) string {
	if s[0] >= '0' && s[0] <= '9' {
		for i := 1; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return "invalid"
			}
		}
		return "number"
	}
	return "ident"
}

func (p *parser) next() token {
	t := p.toks[p.pos]
	p.pos++
	return t
}

func (p *parser) expect(kind, text string) error {
	t := p.next()
	if t.kind == "eof" || t.kind != kind || t.text != text {
		return fmt.Errorf("expected %q, got %q", text, t.text)
	}
	return nil
}

func (p *parser) parseMessages() ([]message, error) {
	var msgs []message
	for {
		t := p.next()
		if t.kind == "eof" {
			return msgs, nil
		}
		if t.kind == "ident" && t.text == "message" {
			m, err := p.parseMessage()
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, m)
			continue
		}
		return nil, fmt.Errorf("expected \"message\", got %q", t.text)
	}
}

func (p *parser) parseMessage() (message, error) {
	m := message{}
	name := p.next()
	if name.kind != "ident" {
		return m, fmt.Errorf("expected message name, got %q", name.text)
	}
	m.name = name.text

	t := p.next()
	if t.kind == "punct" && t.text == "(" {
		num := p.next()
		if num.kind != "number" {
			return m, fmt.Errorf("message %s: expected type number", m.name)
		}
		n, err := parseUint(num.text)
		if err != nil {
			return m, err
		}
		m.typeNumber = n
		if err := p.expect("punct", ")"); err != nil {
			return m, err
		}
		t = p.next()
	}
	if t.kind == "punct" && t.text == "{" {
		for {
			t := p.next()
			if t.kind == "punct" && t.text == "}" {
				return m, nil
			}
			if t.kind == "eof" {
				return m, fmt.Errorf("message %s: unterminated", m.name)
			}
			var f field
			switch {
			case t.kind == "ident" && t.text == "repeated":
				f.repeated = true
				t = p.next()
			case t.kind == "ident" && t.text == "optional":
				f.optional = true
				t = p.next()
			}
			if t.kind != "ident" {
				return m, fmt.Errorf("message %s: expected field type, got %q", m.name, t.text)
			}
			f.typ = t.text
			nameTok := p.next()
			if nameTok.kind != "ident" {
				return m, fmt.Errorf("message %s: expected field name, got %q", m.name, nameTok.text)
			}
			f.name = nameTok.text
			if err := p.expect("punct", "="); err != nil {
				return m, err
			}
			num := p.next()
			if num.kind != "number" {
				return m, fmt.Errorf("message %s field %s: expected number", m.name, f.name)
			}
			n, err := parseUint(num.text)
			if err != nil {
				return m, err
			}
			f.number = n
			if err := p.expect("punct", ";"); err != nil {
				return m, err
			}
			m.fields = append(m.fields, f)
		}
	}
	return m, fmt.Errorf("message %s: expected \"{\" or \"(\"", m.name)
}

// parseUint converts a pure-digit token; the tokenizer guarantees the
// "number" kind is all digits, so errors here are defensive only.
func parseUint(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad number %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// --- validation -----------------------------------------------------------

func validate(msgs []message) error {
	names := map[string]bool{}
	numbers := map[int]string{}
	for _, m := range msgs {
		if names[m.name] {
			return fmt.Errorf("duplicate message %s", m.name)
		}
		names[m.name] = true
		if m.typeNumber != 0 {
			if other, ok := numbers[m.typeNumber]; ok {
				return fmt.Errorf("type number %d used by %s and %s", m.typeNumber, other, m.name)
			}
			numbers[m.typeNumber] = m.name
		}
		fieldNums := map[int]bool{}
		for _, f := range m.fields {
			if _, ok := scalars[f.typ]; !ok && !names[f.typ] {
				return fmt.Errorf("message %s field %s: unknown type %s", m.name, f.name, f.typ)
			}
			if fieldNums[f.number] {
				return fmt.Errorf("message %s: duplicate field number %d", m.name, f.number)
			}
			fieldNums[f.number] = true
			if (f.optional || f.repeated) && (f.typ == "string" || f.typ == "bytes") {
				return fmt.Errorf("message %s field %s: strings/bytes are implicitly optional", m.name, f.name)
			}
			if f.optional && f.repeated {
				return fmt.Errorf("message %s field %s: cannot be both optional and repeated", m.name, f.name)
			}
		}
	}
	return nil
}

// --- code generation --------------------------------------------------------

func generate(msgs []message) ([]byte, error) {
	var b strings.Builder
	b.WriteString("// Code generated by tinypbc from api.pidl. DO NOT EDIT.\n\n")
	b.WriteString("package esphome\n\n")
	b.WriteString("import (\n\t\"errors\"\n\t\"io\"\n\n\t\"github.com/ankon/btproxy/tinypb\"\n)\n\n")

	// Message type constants, sorted by number.
	b.WriteString("// ESPHome API message types.\nconst (\n")
	var ordered []message
	for _, m := range msgs {
		if m.typeNumber != 0 {
			ordered = append(ordered, m)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].typeNumber < ordered[j].typeNumber })
	for _, m := range ordered {
		fmt.Fprintf(&b, "\tMsg%s = %d\n", m.name, m.typeNumber)
	}
	b.WriteString(")\n\n")

	for _, m := range msgs {
		if err := genMessage(&b, m); err != nil {
			return nil, err
		}
	}

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("gofmt: %w\n--- source ---\n%s", err, b.String())
	}
	return src, nil
}

func goName(s string) string {
	if goKeywords[s] {
		return s + "_"
	}
	return s
}

func genMessage(b *strings.Builder, m message) error {
	fmt.Fprintf(b, "// %s is ESPHome API message", m.name)
	if m.typeNumber != 0 {
		fmt.Fprintf(b, " (type %d)", m.typeNumber)
	}
	b.WriteString(".\ntype " + m.name + " struct {\n")
	for _, f := range m.fields {
		fmt.Fprintf(b, "\t%s %s\n", goFieldName(f.name), goFieldType(f))
	}
	b.WriteString("}\n\n")

	genEncode(b, m)
	genDecode(b, m)
	return nil
}

// commonInitialisms keeps generated field names idiomatic (MAC, ID, ...).
var commonInitialisms = map[string]string{
	"api": "API", "id": "ID", "mac": "MAC", "uuid": "UUID", "rssi": "RSSI",
	"ble": "BLE", "hci": "HCI", "esphome": "ESPHome", "ip": "IP", "url": "URL",
	"http": "HTTP", "tcp": "TCP",
}

func goFieldName(name string) string {
	parts := strings.Split(name, "_")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if up, ok := commonInitialisms[p]; ok {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return goName(b.String())
}

func goFieldType(f field) string {
	if s, ok := scalars[f.typ]; ok {
		return s.goType
	}
	if f.repeated {
		return "[]*" + f.typ
	}
	return "*" + f.typ
}

func genEncode(b *strings.Builder, m message) {
	fmt.Fprintf(b, "// Encode returns the protobuf payload of the message.\nfunc (m *%s) Encode() []byte {\n", m.name)
	if len(m.fields) == 0 {
		b.WriteString("\treturn nil\n}\n\n")
		return
	}
	b.WriteString("\tw := tinypb.NewWriter()\n")
	for _, f := range m.fields {
		fn := goFieldName(f.name)
		switch {
		case f.repeated:
			fmt.Fprintf(b, "\tfor i := range m.%s {\n\t\tw.BytesField(%d, m.%s[i].Encode())\n\t}\n", fn, f.number, fn)
		case strings.HasPrefix(goFieldType(f), "*"):
			fmt.Fprintf(b, "\tif m.%s != nil {\n\t\tw.BytesField(%d, m.%s.Encode())\n\t}\n", fn, f.number, fn)
		case f.optional && f.typ == "bool":
			fmt.Fprintf(b, "\tif m.%s {\n\t\tw.Bool(%d, m.%s)\n\t}\n", fn, f.number, fn)
		case f.optional:
			fmt.Fprintf(b, "\tif m.%s != 0 {\n\t\tw.%s(%d, m.%s)\n\t}\n", fn, pbWriteCall(f.typ), f.number, fn)
		default:
			fmt.Fprintf(b, "\tw.%s(%d, m.%s)\n", pbWriteCall(f.typ), f.number, fn)
		}
	}
	b.WriteString("\treturn w.Bytes()\n}\n\n")
}

func pbWriteCall(typ string) string {
	switch typ {
	case "uint32":
		return "Uint32"
	case "uint64":
		return "Uint64"
	case "sint32":
		return "Sint32"
	case "bool":
		return "Bool"
	case "fixed32":
		return "Fixed32"
	case "fixed64":
		return "Fixed64"
	case "float":
		return "Float32"
	case "string":
		return "String"
	case "bytes":
		return "BytesField"
	}
	return "?"
}

func genDecode(b *strings.Builder, m message) {
	fmt.Fprintf(b, "// Decode parses a protobuf payload into the message, skipping unknown\n// fields and wire-type mismatches.\nfunc (m *%s) Decode(payload []byte) error {\n", m.name)
	if len(m.fields) == 0 {
		b.WriteString("\treturn tinypb.NewReader(payload).DiscardAll()\n}\n\n")
		return
	}
	b.WriteString("\tr := tinypb.NewReader(payload)\n\tfor {\n\t\tfield, wire, err := r.Next()\n\t\tif errors.Is(err, io.EOF) {\n\t\t\treturn nil\n\t\t}\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\tswitch field {\n")
	for _, f := range m.fields {
		fn := goFieldName(f.name)
		s, isScalar := scalars[f.typ]
		if !isScalar {
			// Nested message (repeated or single).
			fmt.Fprintf(b, "\t\tcase %d:\n\t\t\tif wire != tinypb.WireBytes {\n\t\t\t\tif err := r.Skip(wire); err != nil {\n\t\t\t\t\treturn err\n\t\t\t\t}\n\t\t\t\tcontinue\n\t\t\t}\n", f.number)
			fmt.Fprintf(b, "\t\t\tbody, err := r.Bytes()\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn err\n\t\t\t\t}\n")
			if f.repeated {
				fmt.Fprintf(b, "\t\t\titem := &%s{}\n\t\t\t\tif err := item.Decode(body); err != nil {\n\t\t\t\t\treturn err\n\t\t\t\t}\n\t\t\t\tm.%s = append(m.%s, item)\n", f.typ, fn, fn)
			} else {
				fmt.Fprintf(b, "\t\t\tm.%s = &%s{}\n\t\t\t\tif err := m.%s.Decode(body); err != nil {\n\t\t\t\t\treturn err\n\t\t\t\t}\n", fn, f.typ, fn)
			}
			continue
		}
		fmt.Fprintf(b, "\t\tcase %d:\n\t\t\tif wire != tinypb.%s {\n\t\t\t\tif err := r.Skip(wire); err != nil {\n\t\t\t\t\treturn err\n\t\t\t\t}\n\t\t\t\tcontinue\n\t\t\t}\n", f.number, s.wire)
		if f.typ == "bytes" {
			fmt.Fprintf(b, "\t\t\tv, err := r.Bytes()\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn err\n\t\t\t\t}\n\t\t\t\tm.%s = append([]byte(nil), v...)\n", fn)
			continue
		}
		fmt.Fprintf(b, "\t\t\tv, err := r.%s()\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn err\n\t\t\t\t}\n\t\t\tm.%s = v\n", s.reader, fn)
	}
	b.WriteString("\t\tdefault:\n\t\t\tif err := r.Skip(wire); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\t}\n\t}\n}\n\n")
}
