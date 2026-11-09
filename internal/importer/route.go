// Package importer talks to the CLI door's import routes and previews a plan
// against them. Nothing here writes except Route.Commit, which the dry run
// never reaches.
package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/smrit-ai/cli/internal/mcp"
)

// Limits of one batch.
const (
	MaxBatchFiles = 25
	MaxBatchBytes = 8 << 20
)

// Upload is one file in a batch: its root relative path, its bytes and what
// the plan says it becomes.
type Upload struct {
	Path  string
	Data  []byte
	Type  string
	Title string
	Slug  string
}

// Similar is a visible document with the same base slug that an import would
// not update.
type Similar struct {
	DocumentID string `json:"document_id"`
	Slug       string `json:"slug"`
	Title      string `json:"title"`
}

// PreviewItem is what a preview says one file would do.
type PreviewItem struct {
	RelativePath   string    `json:"relative_path"`
	Action         string    `json:"action"` // create, update, unchanged, rejected
	Slug           string    `json:"slug"`
	Type           string    `json:"type"`
	TypeFrom       string    `json:"type_from"`
	Title          string    `json:"title"`
	Reason         string    `json:"reason"`
	ReceivedSHA256 string    `json:"received_sha256"`
	DocumentID     string    `json:"document_id"`
	Similar        []Similar `json:"similar"`
}

// CommitItem is what a commit did with one file.
type CommitItem struct {
	RelativePath   string `json:"relative_path"`
	Status         string `json:"status"` // created, updated, unchanged, failed
	DocumentID     string `json:"document_id"`
	Error          string `json:"error"`
	ReceivedSHA256 string `json:"received_sha256"`
}

// RouteError is a refusal from the import route, with a sentence for a
// person. Its status is 403, 404, 413 or 422, and the reason is the route's
// machine readable cause when it gave one.
type RouteError struct {
	Status int
	Reason string
	Msg    string
}

func (e *RouteError) Error() string { return e.Msg }

// Route is the CLI door's import client. It rides the MCP client's HTTP
// rules: the compiled origin, a URL built by concatenation, no redirect, one
// refresh on a 401, 429 waited out, 426 as mcp.ErrUpgrade.
type Route struct{ MCP *mcp.Client }

// Preview asks what importing batch into space would do, and writes nothing.
func (r *Route) Preview(ctx context.Context, space string, batch []Upload) ([]PreviewItem, error) {
	var out struct {
		Items []PreviewItem `json:"items"`
	}
	if err := r.post(ctx, space, "/import/preview", batch, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Commit imports batch into space.
func (r *Route) Commit(ctx context.Context, space string, batch []Upload) ([]CommitItem, error) {
	var out struct {
		Items []CommitItem `json:"items"`
	}
	if err := r.post(ctx, space, "/import", batch, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (r *Route) post(ctx context.Context, space, suffix string, batch []Upload, into any) error {
	if space == "" || len(batch) == 0 {
		return errors.New("Nothing to send.")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	type planEntry struct {
		RelativePath string `json:"relative_path"`
		Type         string `json:"type"`
		Title        string `json:"title"`
		Slug         string `json:"slug"`
	}
	plan := struct {
		Files []planEntry `json:"files"`
	}{Files: []planEntry{}}
	for _, u := range batch {
		w, err := mw.CreateFormFile("files", path.Base(u.Path))
		if err != nil {
			return mcp.ErrUnavailable
		}
		w.Write(u.Data)
		plan.Files = append(plan.Files, planEntry{u.Path, u.Type, u.Title, u.Slug})
	}
	for _, u := range batch {
		mw.WriteField("paths", u.Path)
	}
	pj, _ := json.Marshal(plan)
	mw.WriteField("plan", string(pj))
	if err := mw.Close(); err != nil {
		return mcp.ErrUnavailable
	}
	resp, err := r.MCP.Do(ctx, http.MethodPost, "/cli/v1/spaces/"+url.PathEscape(space)+suffix,
		http.Header{"Content-Type": {mw.FormDataContentType()}, "Accept": {"application/json"}}, buf.Bytes())
	if err != nil {
		return err
	}
	if resp.Status == http.StatusOK {
		if json.Unmarshal(resp.Body, into) != nil {
			return fmt.Errorf("%w Smrit sent an answer this CLI cannot read.", mcp.ErrUnavailable)
		}
		return nil
	}
	switch resp.Status {
	case http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		var e struct {
			Detail string `json:"detail"`
			Reason string `json:"reason"`
		}
		json.Unmarshal(resp.Body, &e)
		msg := plainSentence(e.Detail)
		if resp.Status == http.StatusNotFound || msg == "" {
			msg = map[int]string{
				http.StatusForbidden:             "You cannot import into this space.",
				http.StatusNotFound:              "No space you can see has that name.",
				http.StatusRequestEntityTooLarge: "Smrit refused the batch as too large.",
				http.StatusUnprocessableEntity:   "Smrit refused the batch.",
			}[resp.Status]
		}
		return &RouteError{Status: resp.Status, Reason: plainSentence(e.Reason), Msg: msg}
	}
	return mcp.ErrUnavailable
}

// plainSentence keeps server text to a short line with no control characters.
func plainSentence(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "")
	}
	return s
}
