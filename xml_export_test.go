package main

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type parsedField struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type parsedRow struct {
	Fields []parsedField `xml:",any"`
}

type parsedDocument struct {
	Rows []parsedRow `xml:"row"`
}

func decompressOutput(t *testing.T, rows []map[string]any) string {
	t.Helper()

	var compressed bytes.Buffer
	if err := WriteGzipXML(&compressed, rows); err != nil {
		t.Fatalf("WriteGzipXML() error = %v", err)
	}

	zr, err := gzip.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer zr.Close()

	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("gzip reader Close() error = %v", err)
	}
	return string(data)
}

func parseDocument(t *testing.T, data string) parsedDocument {
	t.Helper()

	var doc parsedDocument
	if err := xml.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatalf("xml.Unmarshal() error = %v\nXML: %s", err, data)
	}
	return doc
}

func fieldMap(row parsedRow) map[string]string {
	fields := make(map[string]string, len(row.Fields))
	for _, field := range row.Fields {
		fields[field.XMLName.Local] = field.Value
	}
	return fields
}

func TestWriteGzipXML(t *testing.T) {
	rows := []map[string]any{
		{
			"hs_code":              "0101210000",
			"standard_import_duty": "0.000%",
			"indent":               6,
			"is_taxable":           true,
			"empty":                "",
			"nil_value":            nil,
			"empty_list":           []any{},
			"empty_map":            map[string]any{},
			"country_of_import":    []any{"AT", "BE"},
			"lc_additional_duties": []map[string]any{
				{
					"duty_name": "Additional duties",
					"lc_duty": []map[string]any{
						{"type": "AD_VALOREM", "value": 50.0, "duty_no": 1},
					},
				},
			},
		},
	}

	want := `<?xml version="1.0" encoding="UTF-8"?><rows><row><country_of_import>["AT","BE"]</country_of_import><empty></empty><empty_list>[]</empty_list><empty_map>{}</empty_map><hs_code>0101210000</hs_code><indent>6</indent><is_taxable>true</is_taxable><lc_additional_duties>[{"duty_name":"Additional duties","lc_duty":[{"duty_no":1,"type":"AD_VALOREM","value":50}]}]</lc_additional_duties><nil_value></nil_value><standard_import_duty>0.000%</standard_import_duty></row></rows>`

	got := decompressOutput(t, rows)
	if got != want {
		t.Fatalf("output mismatch\n got: %s\nwant: %s", got, want)
	}

	doc := parseDocument(t, got)
	if len(doc.Rows) != 1 {
		t.Fatalf("row count = %d, want 1", len(doc.Rows))
	}

	fields := fieldMap(doc.Rows[0])
	if fields["hs_code"] != "0101210000" {
		t.Errorf("hs_code = %q", fields["hs_code"])
	}
	if fields["standard_import_duty"] != "0.000%" {
		t.Errorf("standard_import_duty = %q", fields["standard_import_duty"])
	}
	if fields["country_of_import"] != `["AT","BE"]` {
		t.Errorf("country_of_import = %q", fields["country_of_import"])
	}
	if fields["empty"] != "" {
		t.Errorf("empty = %q", fields["empty"])
	}
	if fields["empty_list"] != "[]" {
		t.Errorf("empty_list = %q", fields["empty_list"])
	}
	if fields["empty_map"] != "{}" {
		t.Errorf("empty_map = %q", fields["empty_map"])
	}
	if fields["nil_value"] != "" {
		t.Errorf("nil_value = %q", fields["nil_value"])
	}
}

func TestWriteGzipXMLEscaping(t *testing.T) {
	value := "a & b < c > d \r\n\t\"quoted\" 'apos'"
	rows := []map[string]any{
		{"value": value, "control": "x\x00y"},
	}

	output := decompressOutput(t, rows)

	if !strings.Contains(output, "&#xD;") {
		t.Errorf("output does not preserve carriage return: %q", output)
	}
	if !strings.Contains(output, `"quoted" 'apos'`) {
		t.Errorf("text quotes were escaped: %q", output)
	}
	if strings.Contains(output, "\x00") {
		t.Errorf("output contains an invalid XML character: %q", output)
	}
	if !strings.Contains(output, "<control>x\uFFFDy</control>") {
		t.Errorf("control character was not replaced: %q", output)
	}

	doc := parseDocument(t, output)
	fields := fieldMap(doc.Rows[0])
	if fields["value"] != value {
		t.Errorf("value = %q, want %q", fields["value"], value)
	}
	if fields["control"] != "x\uFFFDy" {
		t.Errorf("control = %q, want %q", fields["control"], "x\uFFFDy")
	}
}

func TestWriteGzipXMLNestedJSONEscaping(t *testing.T) {
	rows := []map[string]any{
		{
			"nested": map[string]any{
				"text": "a & b < c > d",
			},
		},
	}

	output := decompressOutput(t, rows)
	if !strings.Contains(output, `{"text":"a &amp; b &lt; c &gt; d"}`) {
		t.Errorf("nested JSON was not XML-escaped correctly: %q", output)
	}

	doc := parseDocument(t, output)
	fields := fieldMap(doc.Rows[0])
	if fields["nested"] != `{"text":"a & b < c > d"}` {
		t.Errorf("nested = %q", fields["nested"])
	}
}

func TestSanitizeXMLName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "_"},
		{"valid_name", "valid_name"},
		{"1code", "_1code"},
		{"a b", "a_b"},
		{"a:b", "a_b"},
		{"héllo", "héllo"},
		{"a\x00b", "a_b"},
	}

	for _, tt := range tests {
		if got := sanitizeXMLName(tt.input); got != tt.want {
			t.Errorf("sanitizeXMLName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestWriteGzipXMLSanitizesAndResolvesCollisions(t *testing.T) {
	rows := []map[string]any{
		{"a b": "first", "a:b": "second", "a_b": "third"},
	}

	output := decompressOutput(t, rows)
	doc := parseDocument(t, output)

	if len(doc.Rows[0].Fields) != 3 {
		t.Fatalf("field count = %d, want 3", len(doc.Rows[0].Fields))
	}

	gotNames := make([]string, len(doc.Rows[0].Fields))
	for i, field := range doc.Rows[0].Fields {
		gotNames[i] = field.XMLName.Local
	}
	wantNames := []string{"a_b", "a_b_2", "a_b_3"}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Errorf("field %d name = %q, want %q", i, gotNames[i], wantNames[i])
		}
	}

	fields := fieldMap(doc.Rows[0])
	if fields["a_b"] != "first" {
		t.Errorf("a_b = %q", fields["a_b"])
	}
	if fields["a_b_2"] != "second" {
		t.Errorf("a_b_2 = %q", fields["a_b_2"])
	}
	if fields["a_b_3"] != "third" {
		t.Errorf("a_b_3 = %q", fields["a_b_3"])
	}
}

func TestWriteGzipXMLIsDeterministic(t *testing.T) {
	rows := []map[string]any{
		{
			"z": "last",
			"a": "first",
			"m": []any{"one", "two"},
		},
	}

	want := decompressOutput(t, rows)
	for i := 0; i < 20; i++ {
		if got := decompressOutput(t, rows); got != want {
			t.Fatalf("iteration %d output mismatch\n got: %s\nwant: %s", i, got, want)
		}
	}
}

func TestWriteGzipXMLEmptyRows(t *testing.T) {
	want := `<?xml version="1.0" encoding="UTF-8"?><rows></rows>`
	if got := decompressOutput(t, nil); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestExportGzipXMLFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.xml.gz")
	rows := []map[string]any{{"hs_code": "0101210000"}}

	if err := ExportGzipXMLFile(path, rows); err != nil {
		t.Fatalf("ExportGzipXMLFile() error = %v", err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("os.Open() error = %v", err)
	}
	defer file.Close()

	zr, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer zr.Close()

	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}

	doc := parseDocument(t, string(data))
	if len(doc.Rows) != 1 {
		t.Fatalf("row count = %d, want 1", len(doc.Rows))
	}
	if fields := fieldMap(doc.Rows[0]); fields["hs_code"] != "0101210000" {
		t.Errorf("hs_code = %q", fields["hs_code"])
	}
}

func TestExportGzipXMLFileRemovesPartialFileOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.xml.gz")
	rows := []map[string]any{{"bad": make(chan int)}}

	err := ExportGzipXMLFile(path, rows)
	if err == nil {
		t.Fatal("ExportGzipXMLFile() error = nil, want error")
	}
	if !strings.Contains(err.Error(), `field "bad"`) {
		t.Errorf("error = %q, want field name", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("partial file still exists, os.Stat() error = %v", statErr)
	}
}

func benchmarkRows(n int) []map[string]any {
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{
			"hs_code":              "0101210000",
			"standard_import_duty": "0.000%",
			"country_of_import":    []any{"AT", "BE", "BG", "CY", "CZ", "DK"},
			"is_taxable":           true,
			"indent":               6,
			"lc_additional_duties": []map[string]any{
				{
					"duty_name": "Additional duties",
					"duty":      "50.0000%",
					"lc_duty": []map[string]any{
						{"type": "AD_VALOREM", "value": 50.0, "duty_no": 1},
					},
					"excluding_countries": []map[string]any{
						{"country_code": "BY", "description": "Belarus"},
						{"country_code": "RU", "description": "Russian Federation"},
					},
				},
			},
		}
	}
	return rows
}

func BenchmarkWriteGzipXML(b *testing.B) {
	rows := benchmarkRows(1000)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := WriteGzipXML(io.Discard, rows); err != nil {
			b.Fatal(err)
		}
	}
}
