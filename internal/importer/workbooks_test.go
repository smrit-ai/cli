package importer

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/smrit-ai/cli/internal/mcp"
	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/state"
)

var taskColumns = []plan.Column{
	{Name: "Task", Type: "text"},
	{Name: "Hours", Type: "number"},
	{Name: "Status", Type: "option", Values: []string{"open", "done"}},
	{Name: "Due", Type: "date"},
	{Name: "Paid", Type: "bool"},
}

func workbookEntry(p string) plan.PlanEntry {
	return plan.PlanEntry{Path: p, Kind: "workbook", Space: "engineering", Title: "Tasks",
		Describe: "Every task, one row each.", Columns: taskColumns}
}

// taskCSV is a header and n rows, each padded to about pad bytes.
func taskCSV(n, pad int) string {
	var b strings.Builder
	b.WriteString("Task,Hours,Status,Due,Paid\n")
	for i := 1; i <= n; i++ {
		status := "open"
		if i%2 == 0 {
			status = "done"
		}
		fmt.Fprintf(&b, "\"Task %d, %s\",%d.5,%s,2026-11-%02d,%v\n", i, strings.Repeat("x", pad), i, status, i%28+1, i%3 == 0)
	}
	return b.String()
}

func (f *storeFake) workbookRows() (int, []*fakeWorkbook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeWorkbook
	n := 0
	for _, wb := range f.ext.workbooks {
		out = append(out, wb)
		n += len(wb.Rows)
	}
	return n, out
}

func TestWorkbookBatches200Rows(t *testing.T) {
	root := t.TempDir()
	write(t, root, "tasks.csv", taskCSV(450, 0))
	f := newStore(t)
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{workbookEntry("tasks.csv")}}
	rep := mustRun(t, p, f.opts(t.TempDir()))
	e := entryOf(t, rep, "tasks.csv")
	if e.Status != state.StatusCreated || e.ID == "" || rep.Created != 1 {
		t.Fatalf("%+v", e)
	}

	docs := f.toolCalls("create_document")
	if len(docs) < 1 || docs[0].Args["title"] != "Tasks" || docs[0].Args["body"] != "Every task, one row each." ||
		docs[0].Args["type"] != "reference" || docs[0].Args["space_id"] != "engineering" {
		t.Fatalf("%+v", docs)
	}
	wbs := f.toolCalls("write_workbook")
	if len(wbs) != 1 || wbs[0].Args["document_id"] != e.ID || wbs[0].Args["name"] != "Tasks" {
		t.Fatalf("%+v", wbs)
	}
	ops, _ := json.Marshal(wbs[0].Args["operations"])
	if string(ops) != `[{"name":"Task","op":"add_column","type":"text"},{"name":"Hours","op":"add_column","type":"number"},`+
		`{"name":"Status","op":"add_column","type":"option","values":["open","done"]},{"name":"Due","op":"add_column","type":"date"},`+
		`{"name":"Paid","op":"add_column","type":"bool"}]` {
		t.Fatalf("operations %s", ops)
	}

	var sizes []int
	for _, c := range f.toolCalls("write_records") {
		add, _ := c.Args["add"].([]any)
		sizes = append(sizes, len(add))
	}
	if fmt.Sprint(sizes) != "[200 200 50]" {
		t.Fatalf("batches %v", sizes)
	}
	n, books := f.workbookRows()
	if n != 450 || len(books) != 1 {
		t.Fatalf("%d rows in %d workbooks", n, len(books))
	}
	first, _ := json.Marshal(books[0].Rows[0])
	if string(first) != `{"Due":"2026-11-02","Hours":1.5,"Paid":false,"Status":"open","Task":"Task 1, "}` {
		t.Fatalf("first row %s", first)
	}
	if books[0].Rows[2]["Paid"] != true {
		t.Fatalf("third row %v", books[0].Rows[2])
	}
}

func TestWorkbookBatchesStayUnder1MiB(t *testing.T) {
	root := t.TempDir()
	// 200 rows of about 9 KiB each is about 1.8 MiB: more than one call.
	write(t, root, "tasks.csv", taskCSV(200, 9000))
	f := newStore(t)
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{workbookEntry("tasks.csv")}}
	rep := mustRun(t, p, f.opts(t.TempDir()))
	if e := entryOf(t, rep, "tasks.csv"); e.Status != state.StatusCreated {
		t.Fatalf("%+v", e)
	}
	f.mu.Lock()
	records := f.ext.records
	f.mu.Unlock()
	if len(records) < 2 {
		t.Fatalf("%d calls", len(records))
	}
	for _, r := range records {
		if len(r) >= 1<<20 {
			t.Fatalf("a call of %d bytes", len(r))
		}
	}
	if n, _ := f.workbookRows(); n != 200 {
		t.Fatalf("%d rows", n)
	}
}

// nothingWritten fails if the run wrote anything for a workbook.
func nothingWritten(t *testing.T, f *storeFake) {
	t.Helper()
	for _, tool := range []string{"write_workbook", "write_records"} {
		if n := len(f.toolCalls(tool)); n != 0 {
			t.Fatalf("%s called %d times", tool, n)
		}
	}
	for _, c := range f.toolCalls("create_document") {
		if c.Args["title"] == "Tasks" {
			t.Fatal("the describing document was made")
		}
	}
}

func TestWorkbookHeaderMismatchRefused(t *testing.T) {
	for name, csv := range map[string]string{
		"another order":  "Hours,Task,Status,Due,Paid\n1,a,open,2026-11-01,true\n",
		"a missing name": "Task,Hours,Status,Due\na,1,open,2026-11-01\n",
		"an extra name":  "Task,Hours,Status,Due,Paid,More\na,1,open,2026-11-01,true,x\n",
		"empty":          "",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "tasks.csv", csv)
			f := newStore(t)
			p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{workbookEntry("tasks.csv")}}
			if name == "empty" {
				// An empty file is refused by the plan's own checks or here;
				// either way nothing is written.
				rep, _ := Run(context.Background(), p, f.opts(t.TempDir()))
				if e := entryOf(t, rep, "tasks.csv"); e.Status != state.StatusRefused {
					t.Fatalf("%+v", e)
				}
				nothingWritten(t, f)
				return
			}
			rep := mustRun(t, p, f.opts(t.TempDir()))
			e := entryOf(t, rep, "tasks.csv")
			if e.Status != state.StatusRefused || !strings.Contains(e.Error, "header") {
				t.Fatalf("%+v", e)
			}
			nothingWritten(t, f)

			dry, err := DryRun(context.Background(), Input{Plan: p, MCP: f.client(), Route: &Route{MCP: f.client()}})
			if err != nil || dry.Entries[0].Action != ActionRefused || dry.Entries[0].Reason != e.Error {
				t.Fatalf("dry run %v %+v", err, dry.Entries)
			}
		})
	}
}

// A cell is data: a refusal names the row and the column, never the cell.
func TestWorkbookBadCellRefusedWithoutItsContent(t *testing.T) {
	for name, row := range map[string]string{
		"option": "a,1,SECRETCELL,2026-11-01,true",
		"number": "a,SECRETCELL,open,2026-11-01,true",
		"bool":   "a,1,open,2026-11-01,SECRETCELL",
		"cells":  "a,1,open,SECRETCELL",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "tasks.csv", "Task,Hours,Status,Due,Paid\nb,2,done,2026-11-02,false\n"+row+"\n")
			f := newStore(t)
			p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{workbookEntry("tasks.csv")}}
			rep := mustRun(t, p, f.opts(t.TempDir()))
			e := entryOf(t, rep, "tasks.csv")
			if e.Status != state.StatusRefused || !strings.Contains(e.Error, "Row 2") || strings.Contains(e.Error, "SECRETCELL") {
				t.Fatalf("%+v", e)
			}
			if name != "cells" && !strings.Contains(e.Error, map[string]string{"option": "Status", "number": "Hours", "bool": "Paid"}[name]) {
				t.Fatalf("the column is not named: %s", e.Error)
			}
			out, _ := json.Marshal(rep)
			if strings.Contains(string(out), "SECRETCELL") {
				t.Fatal("a cell reached the report")
			}
			nothingWritten(t, f)
		})
	}
}

func TestWorkbookRerunReportsExisting(t *testing.T) {
	root := t.TempDir()
	write(t, root, "tasks.csv", taskCSV(3, 0))
	f := newStore(t)
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{workbookEntry("tasks.csv")}}
	first := mustRun(t, p, f.opts(t.TempDir()))
	if e := entryOf(t, first, "tasks.csv"); e.Status != state.StatusCreated {
		t.Fatalf("%+v", e)
	}

	again := mustRun(t, p, f.opts(t.TempDir()))
	e := entryOf(t, again, "tasks.csv")
	if e.Status != state.StatusExists || e.Error == "" || e.ID != "" || again.Exists != 1 || again.Created != 0 {
		t.Fatalf("%+v %+v", e, again)
	}
	if len(f.toolCalls("write_workbook")) != 1 || len(f.toolCalls("write_records")) != 1 {
		t.Fatal("a rerun wrote the workbook again")
	}
	if n, books := f.workbookRows(); n != 3 || len(books) != 1 {
		t.Fatalf("%d rows in %d workbooks", n, len(books))
	}
}

// A run that stops with rows unanswered carries on where Smrit's count
// says, so no row is written twice.
func TestWorkbookResumeAfterLostAnswer(t *testing.T) {
	root := t.TempDir()
	write(t, root, "tasks.csv", taskCSV(450, 0))
	f := newStore(t)
	f.ext.loseRecordsAt = 2
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{workbookEntry("tasks.csv")}}
	dir := t.TempDir()
	rep, err := Run(context.Background(), p, f.opts(dir))
	if !errors.Is(err, mcp.ErrUnavailable) || rep.Run == "" {
		t.Fatalf("%v", err)
	}
	if e := entryOf(t, rep, "tasks.csv"); e.Status != state.StatusFailed || e.ID == "" {
		t.Fatalf("%+v", e)
	}

	o := f.opts(dir)
	o.Resume = rep.Run
	again := mustRun(t, p, o)
	if e := entryOf(t, again, "tasks.csv"); e.Status != state.StatusCreated {
		t.Fatalf("%+v", e)
	}
	if n, books := f.workbookRows(); n != 450 || len(books) != 1 {
		t.Fatalf("%d rows in %d workbooks", n, len(books))
	}
	if len(f.toolCalls("create_document")) != 2 || len(f.toolCalls("write_workbook")) != 1 {
		// The describing document once, then the import map.
		t.Fatalf("create_document %d write_workbook %d", len(f.toolCalls("create_document")), len(f.toolCalls("write_workbook")))
	}
}

const shortID = "AbCdEfGhJkLmNpQrStUvW"

var linkColumns = append(append([]plan.Column{}, taskColumns...), plan.Column{Name: "Link", Type: "relation"})

// csvOf writes rows as CSV, quoting as needed.
func csvOf(rows ...[]string) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	w.WriteAll(rows)
	return b.String()
}

var linkHeader = []string{"Task", "Hours", "Status", "Due", "Paid", "Link"}

// goodRow is a row every column accepts; with sets one cell.
func goodRow(col int, v string) []string {
	r := []string{"a", "1", "open", "2026-11-01", "true", "<smrit:d:" + shortID + ">"}
	if col >= 0 {
		r[col] = v
	}
	return r
}

func runLinkCSV(t *testing.T, rows ...[]string) (RunReport, *storeFake, Report) {
	t.Helper()
	root := t.TempDir()
	write(t, root, "tasks.csv", csvOf(append([][]string{linkHeader, goodRow(-1, "")}, rows...)...))
	f := newStore(t)
	e := workbookEntry("tasks.csv")
	e.Columns = linkColumns
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{e}}
	dry, err := DryRun(context.Background(), Input{Plan: p, MCP: f.client(), Route: &Route{MCP: f.client()}})
	if err != nil {
		t.Fatal(err)
	}
	return mustRun(t, p, f.opts(t.TempDir())), f, dry
}

// Every cell Smrit would refuse is refused before anything is written, in
// the dry run and the run alike, naming the row and the column and never
// the cell.
func TestWorkbookCellsSmritRefusesAreRefusedFirst(t *testing.T) {
	cases := []struct {
		name, column string
		row          []string
	}{
		{"blank row", "", []string{"", "", "", "", "", ""}},
		{"blank row of spaces", "", []string{"", " ", "", "  ", "", ""}},
		{"date not a day", "Due", goodRow(3, "2026-02-30")},
		{"date month only", "Due", goodRow(3, "2026-11")},
		{"date no padding", "Due", goodRow(3, "2026-11-9")},
		{"date week 54", "Due", goodRow(3, "2026-W54")},
		{"date week 53 of a 52 week year", "Due", goodRow(3, "2025-W53")},
		{"date week day 8", "Due", goodRow(3, "2026-W45-8")},
		{"date mixed forms", "Due", goodRow(3, "2026W45-1")},
		{"date year 0", "Due", goodRow(3, "0000-01-01")},
		{"date ordinal", "Due", goodRow(3, "2026-313")},
		{"date words", "Due", goodRow(3, "SECRETCELL")},
		{"relation words", "Link", goodRow(5, "SECRETCELL")},
		{"relation bare", "Link", goodRow(5, "smrit:d:"+shortID)},
		{"relation kind", "Link", goodRow(5, "<smrit:x:"+shortID+">")},
		{"relation short id", "Link", goodRow(5, "<smrit:d:SECRETCELL>")},
		{"relation id too large", "Link", goodRow(5, "<smrit:d:zzzzzzzzzzzzzzzzzzzzz>")},
		{"relation seq on a document", "Link", goodRow(5, "<smrit:d:"+shortID+"#3>")},
		{"relation seq leading zero", "Link", goodRow(5, "<smrit:wb:"+shortID+"#03>")},
		{"relation suffix", "Link", goodRow(5, "<smrit:d:"+shortID+">?x")},
		{"relation path", "Link", goodRow(5, "/d/"+shortID)},
		{"text zero width", "Task", goodRow(0, "SECRET​CELL")},
		{"text control", "Task", goodRow(0, "SECRET\x01CELL")},
		{"text lone CR", "Task", goodRow(0, "SECRET\rCELL")},
		{"text soft hyphen", "Task", goodRow(0, "SECRET­CELL")},
		{"text C1", "Task", goodRow(0, "SECRET\u0085CELL")},
		{"text tag", "Task", goodRow(0, "SECRET\U000e0041CELL")},
		{"text variation selector", "Task", goodRow(0, "SECRET︎CELL")},
		{"number overflow", "Hours", goodRow(1, "1e999")},
		{"number negative overflow", "Hours", goodRow(1, "-1e400")},
		{"number NaN", "Hours", goodRow(1, "NaN")},
		{"number Infinity", "Hours", goodRow(1, "Infinity")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep, f, dry := runLinkCSV(t, c.row)
			e := entryOf(t, rep, "tasks.csv")
			if e.Status != state.StatusRefused || !strings.Contains(e.Error, "Row 2") || !strings.Contains(e.Error, c.column) {
				t.Fatalf("%+v", e)
			}
			out, _ := json.Marshal(rep)
			for _, cellText := range c.row {
				if len(cellText) > 4 && strings.Contains(string(out), cellText) {
					t.Fatalf("the cell %q reached the report: %s", cellText, e.Error)
				}
			}
			if strings.Contains(string(out), "SECRET") {
				t.Fatalf("a cell reached the report: %s", e.Error)
			}
			if dry.Entries[0].Action != ActionRefused || dry.Entries[0].Reason != e.Error {
				t.Fatalf("dry run %+v", dry.Entries[0])
			}
			nothingWritten(t, f)
		})
	}
}

// What Smrit accepts is sent, so the check is no stricter than it has to be.
func TestWorkbookCellsSmritAcceptsAreSent(t *testing.T) {
	cases := map[string][]string{
		"date compact":         goodRow(3, "20261109"),
		"date week":            goodRow(3, "2026-W45"),
		"date compact week":    goodRow(3, "2026W451"),
		"date week 53":         goodRow(3, "2020-W53-7"),
		"date padded":          goodRow(3, " 2026-11-09 "),
		"date leap day":        goodRow(3, "2024-02-29"),
		"date year 1":          goodRow(3, "0001-01-01"),
		"relation workbook":    goodRow(5, "<smrit:wb:"+shortID+">"),
		"relation row":         goodRow(5, "<smrit:wb:"+shortID+"#12>"),
		"relation space uuid":  goodRow(5, "<smrit:s:123e4567-e89b-12d3-a456-426614174000>"),
		"relation padded":      goodRow(5, " <smrit:d:"+shortID+"> "),
		"text tab and newline": goodRow(0, "tab\there\r\nand a line"),
		"text emoji":           goodRow(0, "ship it \U0001f680️"),
		"number negative zero": goodRow(1, "-0"),
		"number exponent":      goodRow(1, "1e5"),
		"number long integer":  goodRow(1, "12345678901234567890"),
		"blank but one cell":   {"", "", "", "", "", "<smrit:d:" + shortID + ">"},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			rep, f, dry := runLinkCSV(t, row)
			if e := entryOf(t, rep, "tasks.csv"); e.Status != state.StatusCreated {
				t.Fatalf("%+v", e)
			}
			if dry.Entries[0].Action != ActionCreate {
				t.Fatalf("dry run %+v", dry.Entries[0])
			}
			if n, _ := f.workbookRows(); n != 2 {
				t.Fatalf("%d rows", n)
			}
		})
	}
}

// A row is measured as Smrit stores it: keys c1, c2 and so on, an option's
// value as its key o1, a number as Python writes it, so the limit falls
// where Smrit's does and not where the column names would put it.
func TestWorkbookRowLimitMeasuredAsSmritDoes(t *testing.T) {
	long := strings.Repeat("N", 100)
	cols := []plan.Column{
		{Name: long, Type: "text"},
		{Name: "S", Type: "option", Values: []string{strings.Repeat("v", 200)}},
		{Name: "Num", Type: "number"},
	}
	// {"c1":"x…","c2":"o1","c3":100000.0} is n+33 bytes; 16384 is the limit.
	for n, want := range map[int]string{16351: state.StatusCreated, 16352: state.StatusRefused} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "big.csv", csvOf([]string{long, "S", "Num"}, []string{strings.Repeat("x", n), strings.Repeat("v", 200), "1e5"}))
			f := newStore(t)
			e := plan.PlanEntry{Path: "big.csv", Kind: "workbook", Space: "engineering", Title: "Big", Columns: cols}
			rep := mustRun(t, plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{e}}, f.opts(t.TempDir()))
			if got := entryOf(t, rep, "big.csv"); got.Status != want {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestPythonFloatRepr(t *testing.T) {
	for in, want := range map[string]string{
		"1e5": "100000.0", "1.5": "1.5", "-0.0": "-0.0", "1e16": "1e+16", "1e15": "1000000000000000.0",
		"0.0001": "0.0001", "0.00001": "1e-05", "123.456e10": "1234560000000.0", "1.5e300": "1.5e+300",
		"2.5e-7": "2.5e-07", "12": "12", "-0": "0", "12345678901234567890": "12345678901234567890",
	} {
		if got := storedNumber(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// The answer to write_workbook was lost: the resume finds the workbook on
// its document and writes no second one.
func TestWorkbookResumeAfterLostWorkbookAnswer(t *testing.T) {
	root := t.TempDir()
	write(t, root, "tasks.csv", taskCSV(5, 0))
	f := newStore(t)
	f.ext.loseWorkbook = true
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{workbookEntry("tasks.csv")}}
	dir := t.TempDir()
	rep, err := Run(context.Background(), p, f.opts(dir))
	if !errors.Is(err, mcp.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if e := entryOf(t, rep, "tasks.csv"); e.Status != state.StatusFailed || e.ID == "" {
		t.Fatalf("%+v", e)
	}
	o := f.opts(dir)
	o.Resume = rep.Run
	again := mustRun(t, p, o)
	if e := entryOf(t, again, "tasks.csv"); e.Status != state.StatusCreated {
		t.Fatalf("%+v", e)
	}
	if n, books := f.workbookRows(); n != 5 || len(books) != 1 || len(f.toolCalls("write_workbook")) != 1 {
		t.Fatalf("%d rows in %d workbooks, %d write_workbook", n, len(books), len(f.toolCalls("write_workbook")))
	}
	if len(f.toolCalls("get_document_metadata")) == 0 {
		t.Fatal("the resume did not look for the workbook")
	}
}
