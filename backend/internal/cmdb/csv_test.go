package cmdb_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/cmdb"
)

// What spreadsheets save is read as written: a byte order mark, another separator, quoted cells spanning lines,
// short rows and blank lines, with each row's line kept for problems to point at.
func TestReadCSVTakesWhatSpreadsheetsWrite(t *testing.T) {
	header, rows, err := cmdb.ReadCSV([]byte("\xEF\xBB\xBFname\tdescription\n a \t\"two\nlines\"\n\nb\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(header, "|") != "name|description" || len(rows) != 2 || rows[0].Cell(0) != "a" || rows[0].Cell(1) != "two\nlines" ||
		rows[1].Line != 5 || rows[1].Cell(1) != "" {
		t.Errorf("%q %+v", header, rows)
	}
	for _, bad := range []string{"", "name\n\"open", "name\n\xff\n"} {
		if _, _, err := cmdb.ReadCSV([]byte(bad)); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

// An exported cell a spreadsheet would run as a formula is escaped, and reading it back undoes that.
func TestCSVKeepsFormulasInert(t *testing.T) {
	var buf bytes.Buffer
	if err := cmdb.WriteCSV(&buf, [][]string{{"name", "note"}, {"=HYPERLINK(\"x\")", "-5"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"'=HYPERLINK(""x"")",'-5`) {
		t.Errorf("escaped: %q", buf.String())
	}
	_, rows, err := cmdb.ReadCSV(buf.Bytes())
	if err != nil || rows[0].Cell(0) != `=HYPERLINK("x")` || rows[0].Cell(1) != "-5" {
		t.Errorf("read back: %+v %v", rows, err)
	}
}
