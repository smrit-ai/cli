package importer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// storeExt is the store fake's objects, workbooks and file store. Its MCP
// side runs under the store's lock; the file store takes the same lock.
type storeExt struct {
	f *storeFake
	// trove is the file store, an https server whose client trusts it.
	trove   *httptest.Server
	puts    []troveReq
	objects map[string]*fakeObject
	order   []string // object ids in creation order
	tokens  map[string]string
	nextTok int
	// putStatus answers each PUT in turn; past its end, 204.
	putStatus []int
	// redirectTo, when set, answers every PUT 307 to it.
	redirectTo string
	// uploadURL, when set, is the upload URL handed out instead of the
	// store's own.
	uploadURL func(tok string) string
	// workbooks by id, and every write_records call's raw arguments.
	workbooks map[string]*fakeWorkbook
	records   []json.RawMessage
	// loseRecordsAt answers the write_records call with this number (from
	// 1) 500 after writing its rows, as if the answer were lost.
	loseRecordsAt int
}

type troveReq struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

type fakeObject struct {
	ID, Name, Mime, Scope string
	Data                  []byte
	Arrived               bool
}

type fakeWorkbook struct {
	ID, DocID, Name string
	Operations      []any
	Rows            []map[string]any
}

func newStoreExt(t *testing.T, f *storeFake) *storeExt {
	x := &storeExt{f: f, objects: map[string]*fakeObject{}, tokens: map[string]string{}, workbooks: map[string]*fakeWorkbook{}}
	x.trove = httptest.NewTLSServer(http.HandlerFunc(x.serveTrove))
	t.Cleanup(x.trove.Close)
	return x
}

func (x *storeExt) serveTrove(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	x.puts = append(x.puts, troveReq{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	if x.redirectTo != "" {
		http.Redirect(w, r, x.redirectTo, http.StatusTemporaryRedirect)
		return
	}
	status := http.StatusNoContent
	if len(x.putStatus) > 0 {
		status, x.putStatus = x.putStatus[0], x.putStatus[1:]
	}
	tok := strings.TrimPrefix(r.URL.Path, "/u/")
	id, ok := x.tokens[tok]
	if r.Method != http.MethodPut || !ok {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if status/100 == 2 {
		delete(x.tokens, tok)
		x.objects[id].Data, x.objects[id].Arrived = body, true
	}
	w.WriteHeader(status)
}

// mint is a fresh one use upload URL for object id.
func (x *storeExt) mint(id string) string {
	x.nextTok++
	tok := fmt.Sprintf("uploadtoken%04d", x.nextTok)
	x.tokens[tok] = id
	if x.uploadURL != nil {
		return x.uploadURL(tok)
	}
	return x.trove.URL + "/u/" + tok
}

func (x *storeExt) workbooksOf(docID string) []map[string]string {
	var out []map[string]string
	for _, wb := range x.workbooks {
		if wb.DocID == docID {
			out = append(out, map[string]string{"id": wb.ID, "name": wb.Name})
		}
	}
	return out
}

// call answers the tools the store's own switch does not know.
func (x *storeExt) call(w http.ResponseWriter, name string, a map[string]any, answer func(any), refuse func(string)) bool {
	text := func(s string) {
		b, _ := json.Marshal(s)
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":`+string(b)+`}]}}`)
	}
	switch name {
	case "create_object":
		n, _ := a["name"].(string)
		mt, _ := a["mime_type"].(string)
		sc, _ := a["scope"].(string)
		id := fmt.Sprintf("Obj%018d", len(x.objects)+1)
		x.objects[id] = &fakeObject{ID: id, Name: n, Mime: mt, Scope: sc}
		x.order = append(x.order, id)
		answer(map[string]any{"id": id, "reference": "smrit:object/" + id, "upload_url": x.mint(id),
			"upload_expires_in_seconds": 3600, "note": "Keep the reference in documents."})
	case "get_object":
		id, _ := a["object_id"].(string)
		o := x.objects[id]
		if o == nil {
			refuse("not found")
			return true
		}
		if !o.Arrived {
			text(o.Name + " · waiting for its bytes · send them once with an HTTP PUT to " + x.mint(id))
			return true
		}
		text(fmt.Sprintf("%s · %s · %d bytes · %s · keep smrit:object/%s in documents, not this link", o.Name, o.Mime, len(o.Data), o.Scope, id))
	case "write_workbook":
		doc, _ := a["document_id"].(string)
		n, _ := a["name"].(string)
		for _, wb := range x.workbooks {
			if wb.DocID == doc && wb.Name == n {
				refuse("this document already has a workbook named " + n)
				return true
			}
		}
		ops, _ := a["operations"].([]any)
		id := fmt.Sprintf("Wb%019d", len(x.workbooks)+1)
		x.workbooks[id] = &fakeWorkbook{ID: id, DocID: doc, Name: n, Operations: ops}
		answer(map[string]any{"workbook_id": id, "name": n, "created": true, "columns": []any{}})
	case "write_records":
		raw, _ := json.Marshal(a)
		x.records = append(x.records, raw)
		id, _ := a["workbook_id"].(string)
		wb := x.workbooks[id]
		if wb == nil {
			refuse("not found")
			return true
		}
		add, _ := a["add"].([]any)
		if len(add) > 200 {
			refuse("a call holds at most 200 entries")
			return true
		}
		var seqs []int
		for _, r := range add {
			row, _ := r.(map[string]any)
			wb.Rows = append(wb.Rows, row)
			seqs = append(seqs, len(wb.Rows))
		}
		if x.loseRecordsAt == len(x.records) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		answer(map[string]any{"workbook_id": id, "name": wb.Name, "added": seqs})
	case "query_workbook":
		id, _ := a["workbook_id"].(string)
		wb := x.workbooks[id]
		if wb == nil {
			refuse("not found")
			return true
		}
		answer(map[string]any{"workbook_id": id, "name": wb.Name, "fields": []string{"count"}, "rows": [][]int{{len(wb.Rows)}}, "truncated": false})
	default:
		return false
	}
	return true
}
