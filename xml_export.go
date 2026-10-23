package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	gzipOutputBufferSize = 64 << 10
	xmlBufferSize        = 64 << 10
)

var (
	xmlDeclaration = []byte(`<?xml version="1.0" encoding="UTF-8"?>`)
	jsonNull       = []byte("null")
	xmlReplacement = []byte("\uFFFD")
)

func WriteGzipXML(w io.Writer, rows []map[string]any) (err error) {
	compressed := bufio.NewWriterSize(w, gzipOutputBufferSize)
	zw := gzip.NewWriter(compressed)
	uncompressed := bufio.NewWriterSize(zw, xmlBufferSize)

	defer func() {
		if flushErr := uncompressed.Flush(); err == nil {
			err = flushErr
		}
		if closeErr := zw.Close(); err == nil {
			err = closeErr
		}
		if flushErr := compressed.Flush(); err == nil {
			err = flushErr
		}
	}()

	if _, err = uncompressed.Write(xmlDeclaration); err != nil {
		return err
	}

	xw := newXMLStream(uncompressed)
	if err = xw.writeString("<rows>"); err != nil {
		return err
	}

	sanitizer := newXMLNameSanitizer()
	keys := make([]string, 0, 32)
	names := make([]string, 32)

	for _, row := range rows {
		keys = keys[:0]
		for key := range row {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		if cap(names) < len(keys) {
			names = make([]string, len(keys))
		} else {
			names = names[:len(keys)]
		}
		resolveXMLNames(keys, names, sanitizer)

		if err = xw.writeString("<row>"); err != nil {
			return err
		}
		for i, key := range keys {
			if err = xw.writeElement(key, names[i], row[key]); err != nil {
				return err
			}
		}
		if err = xw.writeString("</row>"); err != nil {
			return err
		}
	}

	if err = xw.writeString("</rows>"); err != nil {
		return err
	}
	return nil
}

func ExportGzipXMLFile(path string, rows []map[string]any) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return err
	}

	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()

	return WriteGzipXML(file, rows)
}

type xmlStream struct {
	w       *bufio.Writer
	jsonBuf bytes.Buffer
	jsonEnc *json.Encoder
	numBuf  [64]byte
}

func newXMLStream(w *bufio.Writer) *xmlStream {
	stream := &xmlStream{w: w}
	stream.jsonEnc = json.NewEncoder(&stream.jsonBuf)
	stream.jsonEnc.SetEscapeHTML(false)
	return stream
}

func (s *xmlStream) writeString(value string) error {
	_, err := s.w.WriteString(value)
	return err
}

func (s *xmlStream) writeElement(key, name string, value any) error {
	if err := s.writeString("<"); err != nil {
		return err
	}
	if err := s.writeString(name); err != nil {
		return err
	}
	if err := s.w.WriteByte('>'); err != nil {
		return err
	}
	if err := s.writeValue(value); err != nil {
		return fmt.Errorf("field %q: %w", key, err)
	}
	if err := s.writeString("</"); err != nil {
		return err
	}
	if err := s.writeString(name); err != nil {
		return err
	}
	return s.w.WriteByte('>')
}

func (s *xmlStream) writeValue(value any) error {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		return s.writeEscapedText(v)
	case bool:
		if v {
			return s.writeString("true")
		}
		return s.writeString("false")
	case int:
		return s.writeInt(int64(v))
	case int8:
		return s.writeInt(int64(v))
	case int16:
		return s.writeInt(int64(v))
	case int32:
		return s.writeInt(int64(v))
	case int64:
		return s.writeInt(v)
	case uint:
		return s.writeUint(uint64(v))
	case uint8:
		return s.writeUint(uint64(v))
	case uint16:
		return s.writeUint(uint64(v))
	case uint32:
		return s.writeUint(uint64(v))
	case uint64:
		return s.writeUint(v)
	case uintptr:
		return s.writeUint(uint64(v))
	case float32:
		return s.writeFloat(float64(v), 32)
	case float64:
		return s.writeFloat(v, 64)
	case json.Number:
		return s.writeEscapedText(v.String())
	default:
		return s.writeJSON(v)
	}
}

func (s *xmlStream) writeInt(value int64) error {
	buf := strconv.AppendInt(s.numBuf[:0], value, 10)
	_, err := s.w.Write(buf)
	return err
}

func (s *xmlStream) writeUint(value uint64) error {
	buf := strconv.AppendUint(s.numBuf[:0], value, 10)
	_, err := s.w.Write(buf)
	return err
}

func (s *xmlStream) writeFloat(value float64, bitSize int) error {
	buf := strconv.AppendFloat(s.numBuf[:0], value, 'g', -1, bitSize)
	_, err := s.w.Write(buf)
	return err
}

func (s *xmlStream) writeJSON(value any) error {
	s.jsonBuf.Reset()
	if err := s.jsonEnc.Encode(value); err != nil {
		return err
	}

	data := s.jsonBuf.Bytes()
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	if bytes.Equal(data, jsonNull) {
		return nil
	}
	return s.writeEscapedBytes(data)
}

func (s *xmlStream) writeEscapedText(value string) error {
	start := 0

	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 {
				if c != '&' && c != '<' && c != '>' {
					i++
					continue
				}
			} else if c == '\t' || c == '\n' {
				i++
				continue
			}

			if start < i {
				if err := s.writeString(value[start:i]); err != nil {
					return err
				}
			}
			if err := s.writeEscapedASCII(c); err != nil {
				return err
			}
			i++
			start = i
			continue
		}

		r, size := utf8.DecodeRuneInString(value[i:])
		if (r == utf8.RuneError && size == 1) || !isValidXMLChar(r) {
			if start < i {
				if err := s.writeString(value[start:i]); err != nil {
					return err
				}
			}
			if _, err := s.w.Write(xmlReplacement); err != nil {
				return err
			}
			i += size
			start = i
			continue
		}
		i += size
	}

	if start < len(value) {
		return s.writeString(value[start:])
	}
	return nil
}

func (s *xmlStream) writeEscapedBytes(value []byte) error {
	start := 0

	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 {
				if c != '&' && c != '<' && c != '>' {
					i++
					continue
				}
			} else if c == '\t' || c == '\n' {
				i++
				continue
			}

			if start < i {
				if _, err := s.w.Write(value[start:i]); err != nil {
					return err
				}
			}
			if err := s.writeEscapedASCII(c); err != nil {
				return err
			}
			i++
			start = i
			continue
		}

		r, size := utf8.DecodeRune(value[i:])
		if (r == utf8.RuneError && size == 1) || !isValidXMLChar(r) {
			if start < i {
				if _, err := s.w.Write(value[start:i]); err != nil {
					return err
				}
			}
			if _, err := s.w.Write(xmlReplacement); err != nil {
				return err
			}
			i += size
			start = i
			continue
		}
		i += size
	}

	if start < len(value) {
		_, err := s.w.Write(value[start:])
		return err
	}
	return nil
}

func (s *xmlStream) writeEscapedASCII(c byte) error {
	switch c {
	case '&':
		return s.writeString("&amp;")
	case '<':
		return s.writeString("&lt;")
	case '>':
		return s.writeString("&gt;")
	case '\r':
		return s.writeString("&#xD;")
	default:
		_, err := s.w.Write(xmlReplacement)
		return err
	}
}

func isValidXMLChar(r rune) bool {
	return r == 0x9 || r == 0xA || r == 0xD ||
		(r >= 0x20 && r <= 0xD7FF) ||
		(r >= 0xE000 && r <= 0xFFFD) ||
		(r >= 0x10000 && r <= 0x10FFFF)
}

type xmlNameSanitizer struct {
	cache map[string]string
}

func newXMLNameSanitizer() *xmlNameSanitizer {
	return &xmlNameSanitizer{cache: make(map[string]string)}
}

func (s *xmlNameSanitizer) sanitize(key string) string {
	if name, ok := s.cache[key]; ok {
		return name
	}
	name := sanitizeXMLName(key)
	s.cache[key] = name
	return name
}

func resolveXMLNames(keys, names []string, sanitizer *xmlNameSanitizer) {
	changed := false
	for i, key := range keys {
		name := sanitizer.sanitize(key)
		names[i] = name
		if name != key {
			changed = true
		}
	}

	if !changed {
		return
	}

	used := make(map[string]struct{}, len(names))
	for i, name := range names {
		candidate := name
		if _, exists := used[candidate]; exists {
			for suffix := 2; ; suffix++ {
				candidate = name + "_" + strconv.Itoa(suffix)
				if _, exists := used[candidate]; !exists {
					break
				}
			}
		}
		used[candidate] = struct{}{}
		names[i] = candidate
	}
}

func sanitizeXMLName(value string) string {
	if value == "" {
		return "_"
	}
	if isValidXMLName(value) {
		return value
	}

	var b strings.Builder
	b.Grow(len(value) + 1)
	first := true

	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('_')
			first = false
			i++
			continue
		}

		if first {
			switch {
			case r != ':' && isXMLNameStart(r):
				b.WriteRune(r)
			case r != ':' && isXMLNameChar(r):
				b.WriteByte('_')
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		} else if r != ':' && isXMLNameChar(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}

		first = false
		i += size
	}

	return b.String()
}

func isValidXMLName(value string) bool {
	if value == "" {
		return false
	}

	first := true
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		if (r == utf8.RuneError && size == 1) || r == ':' {
			return false
		}
		if first {
			if !isXMLNameStart(r) {
				return false
			}
			first = false
		} else if !isXMLNameChar(r) {
			return false
		}
		i += size
	}
	return true
}

func isXMLNameStart(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z':
		return true
	case r == '_':
		return true
	case r >= 'a' && r <= 'z':
		return true
	case r >= 0xC0 && r <= 0xD6:
		return true
	case r >= 0xD8 && r <= 0xF6:
		return true
	case r >= 0xF8 && r <= 0x2FF:
		return true
	case r >= 0x370 && r <= 0x37D:
		return true
	case r >= 0x37F && r <= 0x1FFF:
		return true
	case r >= 0x200C && r <= 0x200D:
		return true
	case r >= 0x2070 && r <= 0x218F:
		return true
	case r >= 0x2C00 && r <= 0x2FEF:
		return true
	case r >= 0x3001 && r <= 0xD7FF:
		return true
	case r >= 0xF900 && r <= 0xFDCF:
		return true
	case r >= 0xFDF0 && r <= 0xFFFD:
		return true
	case r >= 0x10000 && r <= 0xEFFFF:
		return true
	default:
		return false
	}
}

func isXMLNameChar(r rune) bool {
	switch {
	case isXMLNameStart(r):
		return true
	case r == '-':
		return true
	case r == '.':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == 0xB7:
		return true
	case r >= 0x300 && r <= 0x36F:
		return true
	case r >= 0x203F && r <= 0x2040:
		return true
	default:
		return false
	}
}

func main() {
	hsData := []map[string]any{
		{
			"hs_code":                              "$$Section I",
			"hs_code_description_en":               "Live animals; animal products",
			"standard_import_duty":                 []any{},
			"conditional_standard_import_duty":     []any{},
			"origin_based_standard_import_duty":    []any{},
			"additional_duties":                    []any{},
			"standard_deminimis_duty":              []any{},
			"unit_of_measurement":                  0,
			"indent":                               false,
			"is_taxable":                           true,
			"source_hscode":                        []any{"$$Section I"},
			"ancestor_list":                        true,
			"effective_at":                         "2026-10-22",
			"expiry_at":                            "2050-01-01",
			"country_of_import":                    []any{"AT", "BE", "BG", "CY", "CZ", "DK", "EE", "ES", "FI", "FR", "GR", "HR", "HU", "IE", "IT", "LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SE", "SI", "SK", "XI"},
			"wco_legal_notes_path":                 "wcoLegalNotes/2022/EN/V1/SectionI.pdf",
			"wco_explanatory_notes_path":           "wcoExplanatoryNotes/2022/EN/V1/SectionI.pdf",
			"wco_annaxes_path":                     "",
			"country_explanatory_notes_path_en":    "",
			"country_footnotes_path_en":            "",
			"lc_standard_import_duty":              []any{},
			"lc_conditional_standard_import_duty":  []any{},
			"lc_origin_based_standard_import_duty": []any{},
			"lc_additional_duties":                 []any{},
			"lc_standard_deminimis_duty":           []any{},
		},
		{
			"hs_code":                              "01",
			"hs_code_description_en":               "LIVE ANIMALS",
			"standard_import_duty":                 []any{},
			"conditional_standard_import_duty":     []any{},
			"origin_based_standard_import_duty":    []any{},
			"additional_duties":                    []any{},
			"standard_deminimis_duty":              []any{},
			"unit_of_measurement":                  1,
			"indent":                               false,
			"is_taxable":                           true,
			"source_hscode":                        []any{"$$Section I", "01"},
			"ancestor_list":                        true,
			"effective_at":                         "2026-10-22",
			"expiry_at":                            "2050-01-01",
			"country_of_import":                    []any{"AT", "BE", "BG", "CY", "CZ", "DK", "EE", "ES", "FI", "FR", "GR", "HR", "HU", "IE", "IT", "LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SE", "SI", "SK", "XI"},
			"wco_legal_notes_path":                 "wcoLegalNotes/2022/EN/V1/Chapter01.pdf",
			"wco_explanatory_notes_path":           "wcoExplanatoryNotes/2022/EN/V1/Chapter01.pdf",
			"wco_annaxes_path":                     "",
			"country_explanatory_notes_path_en":    "",
			"country_footnotes_path_en":            "",
			"lc_standard_import_duty":              []any{},
			"lc_conditional_standard_import_duty":  []any{},
			"lc_origin_based_standard_import_duty": []any{},
			"lc_additional_duties":                 []any{},
			"lc_standard_deminimis_duty":           []any{},
		},
		{
			"hs_code":                              "0101",
			"hs_code_description_en":               "Live horses, asses, mules and hinnies",
			"standard_import_duty":                 []any{},
			"conditional_standard_import_duty":     []any{},
			"origin_based_standard_import_duty":    []any{},
			"additional_duties":                    []any{},
			"standard_deminimis_duty":              []any{},
			"unit_of_measurement":                  2,
			"indent":                               false,
			"is_taxable":                           true,
			"source_hscode":                        []any{"$$Section I", "01", "0101"},
			"ancestor_list":                        true,
			"effective_at":                         "2026-10-22",
			"expiry_at":                            "2050-01-01",
			"country_of_import":                    []any{"AT", "BE", "BG", "CY", "CZ", "DK", "EE", "ES", "FI", "FR", "GR", "HR", "HU", "IE", "IT", "LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SE", "SI", "SK", "XI"},
			"wco_legal_notes_path":                 "",
			"wco_explanatory_notes_path":           "wcoExplanatoryNotes/2022/EN/V1/Heading0101.pdf",
			"wco_annaxes_path":                     "",
			"country_explanatory_notes_path_en":    "",
			"country_footnotes_path_en":            "countryFootnotes/EU/EN/V1/Chapter0101.pdf",
			"lc_standard_import_duty":              []any{},
			"lc_conditional_standard_import_duty":  []any{},
			"lc_origin_based_standard_import_duty": []any{},
			"lc_additional_duties":                 []any{},
			"lc_standard_deminimis_duty":           []any{},
		},
		{
			"hs_code":                              "0101$$",
			"hs_code_description_en":               "Horses",
			"standard_import_duty":                 []any{},
			"conditional_standard_import_duty":     []any{},
			"origin_based_standard_import_duty":    []any{},
			"additional_duties":                    []any{},
			"standard_deminimis_duty":              []any{},
			"unit_of_measurement":                  3,
			"indent":                               false,
			"is_taxable":                           true,
			"source_hscode":                        []any{"$$Section I", "01", "0101", "0101$$"},
			"ancestor_list":                        true,
			"effective_at":                         "2026-10-22",
			"expiry_at":                            "2050-01-01",
			"country_of_import":                    []any{"AT", "BE", "BG", "CY", "CZ", "DK", "EE", "ES", "FI", "FR", "GR", "HR", "HU", "IE", "IT", "LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SE", "SI", "SK", "XI"},
			"wco_legal_notes_path":                 "",
			"wco_explanatory_notes_path":           "",
			"wco_annaxes_path":                     "",
			"country_explanatory_notes_path_en":    "",
			"country_footnotes_path_en":            "",
			"lc_standard_import_duty":              []any{},
			"lc_conditional_standard_import_duty":  []any{},
			"lc_origin_based_standard_import_duty": []any{},
			"lc_additional_duties":                 []any{},
			"lc_standard_deminimis_duty":           []any{},
		},
		{
			"hs_code":                              "01012",
			"hs_code_description_en":               "Horses :",
			"standard_import_duty":                 []any{},
			"conditional_standard_import_duty":     []any{},
			"origin_based_standard_import_duty":    []any{},
			"additional_duties":                    []any{},
			"standard_deminimis_duty":              []any{},
			"unit_of_measurement":                  4,
			"indent":                               false,
			"is_taxable":                           false,
			"source_hscode":                        []any{"$$Section I", "01", "0101", "0101$$", "01012"},
			"ancestor_list":                        true,
			"effective_at":                         "2026-10-22",
			"expiry_at":                            "2050-01-01",
			"country_of_import":                    []any{"AT", "BE", "BG", "CY", "CZ", "DK", "EE", "ES", "FI", "FR", "GR", "HR", "HU", "IE", "IT", "LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SE", "SI", "SK", "XI"},
			"wco_legal_notes_path":                 "",
			"wco_explanatory_notes_path":           "",
			"wco_annaxes_path":                     "",
			"country_explanatory_notes_path_en":    "",
			"country_footnotes_path_en":            "",
			"lc_standard_import_duty":              []any{},
			"lc_conditional_standard_import_duty":  []any{},
			"lc_origin_based_standard_import_duty": []any{},
			"lc_additional_duties":                 []any{},
			"lc_standard_deminimis_duty":           []any{},
		},
		{
			"hs_code":                              "010121",
			"hs_code_description_en":               "-- Pure-bred breeding animals",
			"standard_import_duty":                 []any{},
			"conditional_standard_import_duty":     []any{},
			"origin_based_standard_import_duty":    []any{},
			"additional_duties":                    []any{},
			"standard_deminimis_duty":              []any{},
			"unit_of_measurement":                  5,
			"indent":                               false,
			"is_taxable":                           false,
			"source_hscode":                        []any{"$$Section I", "01", "0101", "0101$$", "01012", "010121"},
			"ancestor_list":                        true,
			"effective_at":                         "2026-10-22",
			"expiry_at":                            "2050-01-01",
			"country_of_import":                    []any{"AT", "BE", "BG", "CY", "CZ", "DK", "EE", "ES", "FI", "FR", "GR", "HR", "HU", "IE", "IT", "LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SE", "SI", "SK", "XI"},
			"wco_legal_notes_path":                 "",
			"wco_explanatory_notes_path":           "",
			"wco_annaxes_path":                     "",
			"country_explanatory_notes_path_en":    "",
			"country_footnotes_path_en":            "",
			"lc_standard_import_duty":              []any{},
			"lc_conditional_standard_import_duty":  []any{},
			"lc_origin_based_standard_import_duty": []any{},
			"lc_additional_duties":                 []any{},
			"lc_standard_deminimis_duty":           []any{},
		},
		{
			"hs_code":                "0101210000",
			"hs_code_description_en": "Pure-bred breeding animals",
			"standard_import_duty":   "0.0000%",
			"conditional_standard_import_duty": []map[string]any{
				{
					"duty_name":                        "Third country duty",
					"duty":                             "0.0000%",
					"applicable_countries":             []any{"ALL"},
					"applicable_countries_description": "All Countries",
					"excluding_countries":              []any{},
				},
			},
			"origin_based_standard_import_duty": []any{},
			"additional_duties": []map[string]any{
				{
					"duty_name":                        "Additional duties - Y155 - Products exported directly or indirectly from the Russian Federation or Belarus",
					"duty":                             "50.0000%",
					"applicable_countries":             []any{"ALL"},
					"applicable_countries_description": "All Countries",
					"excluding_countries": []map[string]any{
						{"country_code": "BY", "description": "Belarus"},
						{"country_code": "RU", "description": "Russian Federation"},
					},
				},
				{
					"duty_name":                        "Additional duties - No document provided",
					"duty":                             "0.0000%",
					"applicable_countries":             []any{"ALL"},
					"applicable_countries_description": "All Countries",
					"excluding_countries": []map[string]any{
						{"country_code": "BY", "description": "Belarus"},
						{"country_code": "RU", "description": "Russian Federation"},
					},
				},
				{
					"duty_name":                        "Additional duties",
					"duty":                             "50.0000%",
					"applicable_countries":             []any{"BY", "RU"},
					"applicable_countries_description": "Belarus - Russian Federation",
					"excluding_countries":              []any{},
					"check_both_coe_coo":               true,
				},
			},
			"standard_deminimis_duty":           []any{},
			"unit_of_measurement":               []any{"Number of items"},
			"indent":                            6,
			"is_taxable":                        true,
			"source_hscode":                     true,
			"ancestor_list":                     []any{"$$Section I", "01", "0101", "0101$$", "01012", "010121", "0101210000"},
			"is_active":                         true,
			"effective_at":                      "2026-10-22",
			"expiry_at":                         "2050-01-01",
			"country_of_import":                 []any{"AT", "BE", "BG", "CY", "CZ", "DK", "EE", "ES", "FI", "FR", "GR", "HR", "HU", "IE", "IT", "LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SE", "SI", "SK", "XI"},
			"wco_legal_notes_path":              "",
			"wco_explanatory_notes_path":        "",
			"wco_annaxes_path":                  "",
			"country_explanatory_notes_path_en": "countryFootnotes/EU/EN/V1/Commodity0101210000.pdf",
			"country_footnotes_path_en": []map[string]any{
				{
					"duty": "0.0000%",
					"lc_duty": []map[string]any{
						{"type": "AD_VALOREM", "value": 0.0, "duty_no": 1},
					},
					"lc_calculation_formula": "default",
				},
			},
			"lc_standard_import_duty": []map[string]any{
				{
					"duty_name": "Third country duty",
					"duty":      "0.0000%",
					"lc_duty": []map[string]any{
						{"type": "AD_VALOREM", "value": 0.0, "duty_no": 1},
					},
					"lc_calculation_formula": "default",
					"applicable_countries":   []any{"ALL"},
					"excluding_countries":    []any{},
				},
			},
			"lc_conditional_standard_import_duty":  []any{},
			"lc_origin_based_standard_import_duty": []any{},
			"lc_additional_duties": []map[string]any{
				{
					"duty_name": "Additional duties - Y155 - Products exported directly or indirectly from the Russian Federation or Belarus",
					"duty":      "50.0000%",
					"lc_duty": []map[string]any{
						{"type": "AD_VALOREM", "value": 50.0, "duty_no": 1},
					},
					"lc_calculation_formula": "default",
					"applicable_countries":   []any{"ALL"},
					"excluding_countries": []map[string]any{
						{"country_code": "BY", "description": "Belarus"},
						{"country_code": "RU", "description": "Russian Federation"},
					},
				},
				{
					"duty_name": "Additional duties - No document provided",
					"duty":      "0.0000%",
					"lc_duty": []map[string]any{
						{"type": "AD_VALOREM", "value": 0.0, "duty_no": 1},
					},
					"lc_calculation_formula": "default",
					"applicable_countries":   []any{"ALL"},
					"excluding_countries": []map[string]any{
						{"country_code": "BY", "description": "Belarus"},
						{"country_code": "RU", "description": "Russian Federation"},
					},
				},
				{
					"duty_name": "Additional duties",
					"duty":      "50.0000%",
					"lc_duty": []map[string]any{
						{"type": "AD_VALOREM", "value": 50.0, "duty_no": 1},
					},
					"lc_calculation_formula": "default",
					"applicable_countries":   []any{"BY", "RU"},
					"excluding_countries":    []any{},
				},
			},
			"lc_standard_deminimis_duty": []any{},
		},
	}

	if err := ExportGzipXMLFile("hs_data.xml.gz", hsData); err != nil {
		log.Fatal(err)
	}
}
