package importer

import (
	"context"
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
