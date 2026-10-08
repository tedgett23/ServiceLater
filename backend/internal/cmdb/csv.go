package cmdb

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

// CSV import and export (docs/apps-platform.md §8.2). An items file is one class's items, and those of the classes
// extending it (its family), one per row, with a header row naming the columns: the core ones (ItemColumns, with a
// class column saying which class of the family a row is) then the family's fields, by name or by label. A relationships
// file relates items named by class and name. The same format comes out of an export, so a download can be edited
// and uploaded again. Nothing here touches the database.

// Limits of one file.
const (
	MaxImportBytes = 5 << 20
	MaxImportRows  = 10000
)

// The core columns of an items file, in template order. Everything else is a field of the class.
const (
	ColName         = "name"
	ColClass        = "class"
	ColStatus       = "status"
	ColDescription  = "description"
	ColOwnerGroup   = "owner_group"
	ColSupportGroup = "support_group"
)

// ItemColumns are the core columns every items file may have, with the labels a header may use instead. A template
// has the class column only when the class has others extending it.
var ItemColumns = []struct{ Name, Label string }{
	{ColName, "Name"}, {ColClass, "Class"}, {ColStatus, "Status"}, {ColDescription, "Description"},
	{ColOwnerGroup, "Owner group"}, {ColSupportGroup, "Support group"},
}

// RelationshipColumns are a relationships file's columns: "from <type> to", each item named by class and name.
var RelationshipColumns = []string{"from_class", "from_name", "type", "to_class", "to_name"}

// ReadCSV reads a whole file: its header and the rows after it, with cells trimmed. It takes what spreadsheets
// write: a UTF-8 byte order mark, comma, semicolon or tab separators (worked out from the header), and rows shorter
// than the header. Blank rows are skipped; line numbers are kept so problems can point at the row.
func ReadCSV(data []byte) (header []string, rows []Row, err error) {
	data = bytes.TrimPrefix(data, []byte("\uFEFF"))
	if !utf8.Valid(data) {
		return nil, nil, errors.New("the file is not UTF-8 text: save it as CSV UTF-8")
	}
	first, _, _ := bufio.NewReader(bytes.NewReader(data)).ReadLine()
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = sniff(string(first))
	r.FieldsPerRecord = -1
	r.ReuseRecord = false
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				return nil, nil, fmt.Errorf("row %d: %w", pe.StartLine, pe.Err)
			}
			return nil, nil, err
		}
		line, _ := r.FieldPos(0)
		for i := range rec {
			rec[i] = unescapeFormula(strings.TrimSpace(rec[i]))
		}
		if header == nil {
			header = rec
			continue
		}
		if !slices.ContainsFunc(rec, func(s string) bool { return s != "" }) {
			continue
		}
		if len(rows) == MaxImportRows {
			return nil, nil, fmt.Errorf("the file has more than %d rows: split it", MaxImportRows)
		}
		rows = append(rows, Row{Line: line, Cells: rec})
	}
	if header == nil {
		return nil, nil, errors.New("the file is empty: it needs a header row naming the columns")
	}
	return header, rows, nil
}

// unescapeFormula undoes WriteCSV's guard, so an export uploads again unchanged.
func unescapeFormula(s string) string {
	if len(s) > 1 && s[0] == '\'' && strings.ContainsRune("=+-@", rune(s[1])) {
		return s[1:]
	}
	return s
}

// Row is one row of a file and the line it starts on.
type Row struct {
	Line  int
	Cells []string
}

// Cell is the row's value in a column, or "" when the row is short.
func (r Row) Cell(col int) string {
	if col < 0 || col >= len(r.Cells) {
		return ""
	}
	return r.Cells[col]
}

// sniff picks the separator a header row uses: the most common of comma, semicolon and tab.
func sniff(header string) rune {
	best, n := ',', strings.Count(header, ",")
	for _, c := range []rune{';', '\t'} {
		if m := strings.Count(header, string(c)); m > n {
			best, n = c, m
		}
	}
	return best
}

// headerKey is how a header cell is matched: case and the difference between spaces and underscores aside.
func headerKey(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "_")
}

// ItemLayout is which column of an items file holds what: Core by core column name, Fields by field name.
type ItemLayout struct {
	Core   map[string]int
	Fields map[string]int
}

// FamilyFields are the fields of a family's classes (the class first, then those extending it), each name once: the
// class's own and inherited ones, then each descendant's additions.
func FamilyFields(family []*apps.Class) []apps.FormField {
	var out []apps.FormField
	seen := map[string]bool{}
	for _, c := range family {
		for _, f := range c.All {
			if !seen[f.Name] {
				seen[f.Name] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// ItemHeader matches an items file's header to the family of the file's class (family[0] and the classes extending
// it): each cell is a core column or one of the family's fields, by name or label. Anything else, a column twice, or
// no name column is refused, naming the culprits, so a typo in a header never quietly drops a column.
func ItemHeader(family []*apps.Class, header []string) (ItemLayout, error) {
	c := family[0]
	known := map[string]string{} // header key -> core column or "field:name"
	for _, col := range ItemColumns {
		known[headerKey(col.Name)], known[headerKey(col.Label)] = col.Name, col.Name
	}
	for _, f := range FamilyFields(family) {
		if _, taken := known[headerKey(f.Label)]; !taken {
			known[headerKey(f.Label)] = "field:" + f.Name
		}
		known[headerKey(f.Name)] = "field:" + f.Name
	}
	out := ItemLayout{Core: map[string]int{}, Fields: map[string]int{}}
	var unknown, twice []string
	for i, h := range header {
		if h == "" {
			continue
		}
		target, ok := known[headerKey(h)]
		if !ok {
			unknown = append(unknown, strconv.Quote(h))
			continue
		}
		m, key := out.Core, target
		if name, isField := strings.CutPrefix(target, "field:"); isField {
			m, key = out.Fields, name
		}
		if _, dup := m[key]; dup {
			twice = append(twice, strconv.Quote(h))
			continue
		}
		m[key] = i
	}
	switch {
	case len(unknown) > 0:
		return out, fmt.Errorf("%s items have no column %s: the template lists the columns", c.Label, strings.Join(unknown, ", "))
	case len(twice) > 0:
		return out, fmt.Errorf("the column %s appears twice", strings.Join(twice, ", "))
	}
	if _, ok := out.Core[ColName]; !ok {
		return out, errors.New("the file needs a name column")
	}
	return out, nil
}

// RelationshipHeader matches a relationships file's header: every column, each once, in any order.
func RelationshipHeader(header []string) (map[string]int, error) {
	out := map[string]int{}
	for i, h := range header {
		k := headerKey(h)
		if k == "" {
			continue
		}
		if !slices.Contains(RelationshipColumns, k) {
			return nil, fmt.Errorf("a relationships file has no column %q: its columns are %s", h, strings.Join(RelationshipColumns, ", "))
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("the column %q appears twice", h)
		}
		out[k] = i
	}
	for _, c := range RelationshipColumns {
		if _, ok := out[c]; !ok {
			return nil, fmt.Errorf("the file needs a %s column", c)
		}
	}
	return out, nil
}

// FieldCell turns a cell into a value of the field's type, as ApplyFields takes it: whole numbers; yes/no, true/false
// or 1/0; a choice's value or its label; a date as YYYY-MM-DD; a date and time as RFC 3339 or "YYYY-MM-DD HH:MM",
// taken as UTC. The message says what the cell should hold.
func FieldCell(f *apps.FormField, s string) (any, string) {
	switch f.Type {
	case apps.FieldNumber:
		n, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
		if err != nil {
			return nil, "takes a whole number"
		}
		return n, ""
	case apps.FieldBoolean:
		switch strings.ToLower(s) {
		case "true", "yes", "y", "1":
			return true, ""
		case "false", "no", "n", "0":
			return false, ""
		}
		return nil, "takes yes or no"
	case apps.FieldChoice:
		for _, o := range f.Options {
			if strings.EqualFold(o.Value, s) || strings.EqualFold(o.Label, s) {
				return o.Value, ""
			}
		}
		labels := make([]string, len(f.Options))
		for i, o := range f.Options {
			labels[i] = o.Label
		}
		return nil, "must be one of " + strings.Join(labels, ", ")
	case apps.FieldDate:
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			return nil, "takes a date as YYYY-MM-DD (a spreadsheet may have reformatted it: format the column as text)"
		}
		return s, ""
	case apps.FieldDateTime:
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), ""
			}
		}
		return nil, "takes a date and time as YYYY-MM-DD HH:MM (UTC) or RFC 3339"
	}
	return s, ""
}

// StatusCell is an item status from a cell: its value or its label.
func StatusCell(s string) (string, bool) {
	for _, o := range apps.CIStatuses {
		if strings.EqualFold(o.Value, s) || strings.EqualFold(o.Label, s) {
			return o.Value, true
		}
	}
	return "", false
}

// FieldText is a stored field value as a cell, the way FieldCell reads it back.
func FieldText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return fmt.Sprint(v)
}

// ItemTemplateHeader is the header row of a family's items file: the core columns (class only when the family has
// more than the one class), then the family's fields, by name.
func ItemTemplateHeader(family []*apps.Class) []string {
	var out []string
	for _, col := range ItemColumns {
		if col.Name != ColClass || len(family) > 1 {
			out = append(out, col.Name)
		}
	}
	for _, f := range FamilyFields(family) {
		out = append(out, f.Name)
	}
	return out
}

// WriteCSV writes rows as CSV with a byte order mark, so a spreadsheet opens it as UTF-8. A cell a spreadsheet would
// run as a formula (=, +, -, @ first) is prefixed with a quote mark, so an exported value never executes.
func WriteCSV(w io.Writer, rows [][]string) error {
	if _, err := io.WriteString(w, "\uFEFF"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, c := range row {
			if c != "" && strings.ContainsRune("=+-@\t\r", rune(c[0])) {
				c = "'" + c
			}
			safe[i] = c
		}
		if err := cw.Write(safe); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
