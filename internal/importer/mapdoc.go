package importer

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/smrit-ai/cli/internal/mcp"
	"github.com/smrit-ai/cli/internal/scan"
	"github.com/smrit-ai/cli/internal/state"
)

// Sentences about the import map.
const (
	mapDuplicate = "A document with the import map's name is already in that space, so no second one was made."
	mapUnread    = "Smrit sent an answer about the import map this CLI cannot read."
)

// importMap writes one document listing every imported path with its
// mention, in the space of the plan's first imported document. A run that
// has made its map updates it; it never makes a second.
func (r *runner) importMap() error {
	body, space := r.mapBody()
	if body == "" {
		return nil
	}
	note := "Written by smrit import, run " + r.st.ID + "."
	if r.st.Map != "" {
		// A map that already says this gets no new revision.
		raw, err := r.o.MCP.Call(r.ctx, "get_document", map[string]any{"document_id": r.st.Map})
		if err != nil {
			return r.mapRefused(err)
		}
		if current, ok := bodyOf(raw); ok && strings.TrimSpace(current) == strings.TrimSpace(body) {
			return nil
		}
		raw, err = r.o.MCP.Call(r.ctx, "get_document_metadata", map[string]any{"document_id": r.st.Map})
		if err != nil {
			return r.mapRefused(err)
		}
		var meta struct {
			Revision *int `json:"revision"`
		}
		if json.Unmarshal(raw, &meta) != nil || meta.Revision == nil {
			r.mapError = mapUnread
			return nil
		}
		_, err = r.o.MCP.Call(r.ctx, "update_document", map[string]any{
			"document_id": r.st.Map, "base_revision": *meta.Revision, "body": body, "change_note": note,
		})
		return r.mapRefused(err)
	}
	now := r.o.Now
	if now == nil {
		now = timeNow
	}
	title := "Import map: " + plainSentence(filepath.Base(r.p.Root)) + " " + now().Format("2006-01-02")
	raw, err := r.o.MCP.Call(r.ctx, "create_document", map[string]any{
		"space_id": space, "title": title, "body": body, "type": "reference", "change_note": note,
	})
	if err != nil {
		var te *mcp.ToolError
		if errors.As(err, &te) && strings.Contains(te.Message, "already has a document named") {
			r.mapError = mapDuplicate
			return nil
		}
		return r.mapRefused(err)
	}
	var made struct {
		DocumentID string `json:"document_id"`
	}
	if json.Unmarshal(raw, &made) != nil || made.DocumentID == "" {
		r.mapError = mapUnread
		return nil
	}
	r.st.Map = made.DocumentID
	return r.save()
}

// mapRefused keeps a refusal of the map in the report; anything else, such
// as Smrit being out of reach or this CLI too old, stops the run.
func (r *runner) mapRefused(err error) error {
	var te *mcp.ToolError
	if errors.As(err, &te) {
		r.mapError = "The import map could not be written: " + plainSentence(te.Message)
		return nil
	}
	return err
}

// bodyOf is get_document's answer: the body as a string, or wrapped as
// {"result": body} when the server sent it structured.
func bodyOf(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var w struct {
		Result *string `json:"result"`
	}
	if json.Unmarshal(raw, &w) == nil && w.Result != nil {
		return *w.Result, true
	}
	return "", false
}

// mapBody is the map's Markdown table, in plan order, and the space it goes
// in: every entry Smrit holds from this run, including one whose file is no
// longer in the folder. Empty when nothing was imported.
func (r *runner) mapBody() (body, space string) {
	valid := map[string]string{}
	for _, e := range r.pr.entries {
		valid[e.Path] = e.Space
	}
	seen := map[string]bool{}
	var b strings.Builder
	for _, e := range r.p.Entries {
		s := r.st.Entries[e.Path]
		if seen[e.Path] || !state.Done(s.Status) || s.ID == "" {
			continue
		}
		seen[e.Path] = true
		if space == "" && e.Kind == scan.KindDocument {
			if space = valid[e.Path]; space == "" {
				space = e.Space
			}
		}
		ref := "<smrit:d:" + s.ID + ">"
		if e.Kind == scan.KindObject {
			ref = "smrit:object/" + s.ID
		}
		b.WriteString("| " + escapeCell(e.Path) + " | " + ref + " |\n")
	}
	if space == "" {
		return "", ""
	}
	return "| Path | Document |\n|---|---|\n" + b.String(), space
}

// escapeCell keeps a path one plain table cell: no column break, no
// emphasis, no link, no code, no tag.
func escapeCell(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if strings.IndexByte("\\|`*_[]<>", s[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
