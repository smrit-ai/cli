package importer

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/smrit-ai/cli/internal/mcp"
	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/state"
)

// Limits of a workbook import. The row, column and batch limits are
// Smrit's own; the file limit bounds what is held in memory.
const (
	MaxWorkbookBytes = 64 << 20
	maxWorkbookRows  = 50_000
	maxColumns       = 100
	maxRowBytes      = 16_384
	maxRecordsBatch  = 200
	// maxRecordsBytes keeps one write_records call under Smrit's 1 MiB
	// request limit with room for the rest of the request.
	maxRecordsBytes = 1<<20 - 64<<10
)

var columnTypes = map[string]bool{"text": true, "number": true, "date": true, "bool": true, "option": true, "relation": true}

// Sentences about workbooks. A cell is data, so none of them carries one.
const (
	wbTooBig      = "This CSV is over 64 MiB, the most smrit import reads as one workbook."
	wbEmpty       = "The CSV is empty, so it has no header to match the plan's columns."
	wbHeader      = "The CSV's header does not match the plan's columns, name for name and in order."
	wbNoColumns   = "A workbook needs its columns in the plan."
	wbTooManyRows = "The CSV has more than 50,000 rows, the most one workbook holds."
	wbExists      = "Smrit already has a document with this workbook's title in this space, so nothing was written for it. Run smrit import --resume with the earlier run to carry that one on."
	wbUnread      = "Smrit sent an answer about this workbook this CLI cannot read."
	wbUnfinished  = "The workbook's rows had not all reached Smrit when the run stopped."
	wbChanged     = "This file changed since this run imported it. Smrit keeps the rows it was sent."
	wbChangedPart = "This file changed after part of it reached Smrit, so the rest was not sent."
	wbMoreRows    = "The workbook in Smrit holds more rows than this CSV, so nothing more was sent."
)

// readWorkbook reads the CSV the rescan listed and turns every row into the
// values write_records takes, checking every cell first. A file that does
// not fit the plan is refused, with one sentence, before anything is
// written.
func readWorkbook(e plan.Entry) ([]map[string]any, string) {
	if reason := checkColumns(e.Columns); reason != "" {
		return nil, reason
	}
	if e.Size > MaxWorkbookBytes {
		return nil, wbTooBig
	}
	data, err := readExact(e)
	if err != nil {
		return nil, err.Error()
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err == io.EOF {
		return nil, wbEmpty
	}
	if err != nil {
		return nil, csvError(err)
	}
	if len(header) != len(e.Columns) {
		return nil, wbHeader
	}
	for i, c := range e.Columns {
		if header[i] != c.Name {
			return nil, wbHeader
		}
	}
	var rows []map[string]any
	for n := 1; ; n++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, csvError(err)
		}
		if n > maxWorkbookRows {
			return nil, wbTooManyRows
		}
		if len(rec) != len(header) {
			return nil, fmt.Sprintf("Row %d under the header has %d cells and the header has %d.", n, len(rec), len(header))
		}
		row := map[string]any{}
		for i, c := range e.Columns {
			v, ok, reason := cell(rec[i], c)
			if reason != "" {
				return nil, fmt.Sprintf("Row %d under the header has a value in the %s column that %s.", n, columnName(c.Name), reason)
			}
			if ok {
				row[c.Name] = v
			}
		}
		if rowBytes(row) > maxRowBytes {
			return nil, fmt.Sprintf("Row %d under the header is over 16 KiB, the most one workbook row holds.", n)
		}
		rows = append(rows, row)
	}
	return rows, ""
}

// checkColumns refuses columns Smrit would refuse, before anything is
// written for the workbook.
func checkColumns(cols []plan.Column) string {
	if len(cols) == 0 {
		return wbNoColumns
	}
	if len(cols) > maxColumns {
		return "A workbook holds at most 100 columns."
	}
	seen := map[string]bool{}
	for _, c := range cols {
		switch {
		case strings.TrimSpace(c.Name) == "":
			return "A column in the plan has no name."
		case seen[c.Name]:
			return fmt.Sprintf("The plan names the %s column twice.", columnName(c.Name))
		case !columnTypes[c.Type]:
			return fmt.Sprintf("The %s column's type is not text, number, date, bool, option or relation.", columnName(c.Name))
		case c.Type == "option" && len(c.Values) == 0:
			return fmt.Sprintf("The %s column is an option column and the plan gives it no values.", columnName(c.Name))
		case c.Type != "option" && len(c.Values) > 0:
			return fmt.Sprintf("The %s column has values and is not an option column.", columnName(c.Name))
		}
		seen[c.Name] = true
	}
	return ""
}

// cell is one CSV cell as its column's type: ok false leaves it out, which
// Smrit stores as no value. reason, when set, ends a sentence saying why
// the cell does not fit; it never repeats the cell.
func cell(s string, c plan.Column) (v any, ok bool, reason string) {
	t := strings.TrimSpace(s)
	if c.Type == "text" {
		return s, s != "", ""
	}
	if t == "" {
		return nil, false, ""
	}
	switch c.Type {
	case "number":
		if (t[0] != '-' && (t[0] < '0' || t[0] > '9')) || !json.Valid([]byte(t)) {
			return nil, false, "is not a number"
		}
		return json.Number(t), true, ""
	case "bool":
		switch strings.ToLower(t) {
		case "true":
			return true, true, ""
		case "false":
			return false, true, ""
		}
		return nil, false, "is not true or false"
	case "option":
		for _, x := range c.Values {
			if x == t {
				return t, true, ""
			}
		}
		return nil, false, "is not one of the column's values"
	}
	return t, true, ""
}

// rowBytes is a row's size as Smrit counts it: compact JSON, unescaped.
func rowBytes(row map[string]any) int {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(row)
	return b.Len() - 1
}

func csvError(err error) string {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return fmt.Sprintf("The CSV could not be read at line %d.", pe.Line)
	}
	return "The CSV could not be read."
}

// columnName is a column's name from the plan for a sentence, quoted and
// kept short.
func columnName(s string) string {
	s = plainSentence(s)
	if utf8.RuneCountInString(s) > 60 {
		s = string([]rune(s)[:60])
	}
	return fmt.Sprintf("%q", s)
}

// recordBatches cuts rows into write_records calls of at most 200 rows and
// under 1 MiB of JSON each, measured as the client sends it.
func recordBatches(rows []map[string]any) [][]map[string]any {
	var out [][]map[string]any
	var cur []map[string]any
	size := 2
	for _, row := range rows {
		b, _ := json.Marshal(row)
		n := len(b) + 1
		if len(cur) > 0 && (len(cur) >= maxRecordsBatch || size+n > maxRecordsBytes) {
			out = append(out, cur)
			cur, size = nil, 2
		}
		cur = append(cur, row)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// workbooks imports each CSV entry: its describing document, the workbook
// with the plan's columns, then its rows in batches. Progress is saved
// after each step, so a resume carries on without a second document,
// workbook or row.
func (r *runner) workbooks(es []plan.Entry) error {
	for _, e := range es {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if err := r.workbook(e); err != nil {
			return err
		}
		if err := r.save(); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) workbook(e plan.Entry) error {
	prev := r.st.Entries[e.Path]
	if state.Done(prev.Status) && prev.ID != "" {
		o := &r.out[e.Index]
		o.Status, o.ID = prev.Status, prev.ID
		if prev.SourceSHA256 != e.SHA256 {
			o.Error = wbChanged
		}
		return nil
	}
	// Anything this run made for the entry earlier, from the same bytes, is
	// carried on.
	st := state.EntryState{Kind: e.Kind, SourceSHA256: e.SHA256, Status: state.StatusFailed, Error: wbUnfinished, Phase: r.st.Phase}
	resumed := prev.Status == state.StatusFailed && prev.ID != ""
	if resumed && prev.SourceSHA256 != e.SHA256 {
		r.failWithID(e, wbChangedPart, prev.ID)
		return nil
	}
	rows, reason := readWorkbook(e)
	if reason != "" {
		if resumed {
			r.failWithID(e, reason, prev.ID)
		} else {
			r.refuse(e, reason)
		}
		return nil
	}
	if resumed {
		st.ID, st.Workbook, st.Rows = prev.ID, prev.Workbook, prev.Rows
	}
	keep := func() error {
		r.record(e, st)
		return r.save()
	}
	failed := func(msg string) error {
		st.Error = msg
		r.record(e, st)
		return nil
	}
	refused := func(err error, what string) error {
		var te *mcp.ToolError
		if !errors.As(err, &te) {
			return err
		}
		return failed(what)
	}

	if st.ID == "" {
		typ := e.Type
		if typ == "" {
			typ = "reference"
		}
		body := e.Describe
		if strings.TrimSpace(body) == "" {
			body = "Rows imported from " + e.Path + " by smrit import."
		}
		raw, err := r.o.MCP.Call(r.ctx, "create_document", map[string]any{
			"space_id": e.Space, "title": e.Title, "body": body, "type": typ,
			"change_note": "Written by smrit import, run " + r.st.ID + ".",
		})
		if err != nil {
			var te *mcp.ToolError
			if errors.As(err, &te) && strings.Contains(te.Message, "already has a document named") {
				r.record(e, state.EntryState{Kind: e.Kind, SourceSHA256: e.SHA256, Status: state.StatusExists, Error: wbExists, Phase: r.st.Phase})
				return nil
			}
			if errors.As(err, &te) {
				return failed("Smrit refused the workbook's document: " + plainSentence(te.Message))
			}
			return err
		}
		var made struct {
			DocumentID string `json:"document_id"`
		}
		if json.Unmarshal(raw, &made) != nil || made.DocumentID == "" {
			return failed(wbUnread)
		}
		st.ID = made.DocumentID
		r.ix.ids[e.Path] = st.ID
		if err := keep(); err != nil {
			return err
		}
	}
	r.ix.ids[e.Path] = st.ID

	if st.Workbook == "" && resumed {
		// The answer to an earlier write_workbook may have been lost.
		id, err := r.attachedWorkbook(st.ID, e.Title)
		if err != nil {
			return refused(err, "Smrit refused a look at the workbook's document.")
		}
		st.Workbook = id
	}
	if st.Workbook == "" {
		ops := make([]map[string]any, 0, len(e.Columns))
		for _, c := range e.Columns {
			op := map[string]any{"op": "add_column", "name": c.Name, "type": c.Type}
			if c.Type == "option" {
				op["values"] = c.Values
			}
			ops = append(ops, op)
		}
		raw, err := r.o.MCP.Call(r.ctx, "write_workbook", map[string]any{"document_id": st.ID, "name": e.Title, "operations": ops})
		if err != nil {
			var te *mcp.ToolError
			if errors.As(err, &te) {
				return failed("Smrit refused the workbook: " + plainSentence(te.Message))
			}
			return err
		}
		var made struct {
			WorkbookID string `json:"workbook_id"`
		}
		if json.Unmarshal(raw, &made) != nil || made.WorkbookID == "" {
			return failed(wbUnread)
		}
		st.Workbook, st.Rows = made.WorkbookID, 0
		if err := keep(); err != nil {
			return err
		}
	} else if resumed {
		// Rows are appended in order and each call is all or nothing, so
		// Smrit's count says where to carry on, even after a lost answer.
		n, err := r.rowCount(st.Workbook)
		if err != nil {
			return refused(err, "Smrit refused a count of the workbook's rows.")
		}
		if n < 0 {
			return failed(wbUnread)
		}
		if n > len(rows) {
			return failed(wbMoreRows)
		}
		st.Rows = n
	}

	for _, batch := range recordBatches(rows[st.Rows:]) {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		_, err := r.o.MCP.Call(r.ctx, "write_records", map[string]any{"workbook_id": st.Workbook, "add": batch})
		if err != nil {
			// Smrit's refusal may quote a cell, so its words are not kept.
			return refused(err, fmt.Sprintf("Smrit refused rows %d to %d, so the workbook holds only the rows before them.", st.Rows+1, st.Rows+len(batch)))
		}
		st.Rows += len(batch)
		if err := keep(); err != nil {
			return err
		}
	}
	st.Status, st.Error = state.StatusCreated, ""
	r.record(e, st)
	return nil
}

// attachedWorkbook is the id of the workbook named name on the document, or
// "" when it has none.
func (r *runner) attachedWorkbook(docID, name string) (string, error) {
	raw, err := r.o.MCP.Call(r.ctx, "get_document_metadata", map[string]any{"document_id": docID})
	if err != nil {
		return "", err
	}
	var meta struct {
		Workbooks []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"workbooks"`
	}
	json.Unmarshal(raw, &meta)
	for _, w := range meta.Workbooks {
		if w.Name == name {
			return w.ID, nil
		}
	}
	return "", nil
}

// rowCount is how many rows the workbook holds, or -1 when the answer
// cannot be read.
func (r *runner) rowCount(id string) (int, error) {
	raw, err := r.o.MCP.Call(r.ctx, "query_workbook", map[string]any{"workbook_id": id, "aggregate": []map[string]any{{"fn": "count"}}})
	if err != nil {
		return 0, err
	}
	var ans struct {
		Rows [][]json.Number `json:"rows"`
	}
	if json.Unmarshal(raw, &ans) != nil || len(ans.Rows) != 1 || len(ans.Rows[0]) != 1 {
		return -1, nil
	}
	n, err := ans.Rows[0][0].Int64()
	if err != nil || n < 0 {
		return -1, nil
	}
	return int(n), nil
}
