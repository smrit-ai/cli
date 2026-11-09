package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/smrit-ai/cli/internal/mcp"
	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/server"
	"github.com/smrit-ai/cli/internal/state"
)

// MaxObjectBytes is the largest file smrit import stores as an object. The
// whole file is held in memory, so it can be hashed against the rescan and
// sent as exactly the bytes that were checked.
const MaxObjectBytes = 100 << 20

// uploadTimeout bounds one upload, start to finish.
const uploadTimeout = 15 * time.Minute

// Sentences about objects. None names an upload URL: it is a credential.
const (
	objectTooBig     = "This file is over 100 MiB, the most smrit import stores as one object."
	objectBadType    = "The plan's type for this object is not a MIME type such as image/png."
	objectUnread     = "Smrit sent an answer about this file this CLI cannot read."
	objectBadURL     = "Smrit gave an upload address on a host this CLI does not send files to, so the file was not sent."
	objectNotSent    = "The file's bytes could not be sent to Smrit's file store."
	objectNoURL      = "Smrit gave no way to send this file's bytes."
	objectTooLate    = "Smrit no longer takes this file's bytes. Run smrit import without --resume to store it as a new object."
	objectUnfinished = "The file's bytes had not reached Smrit when the run stopped."
	objectChanged    = "This file changed since this run stored it. Smrit keeps the bytes it was sent."
)

// mimeRE is the server's own rule for a MIME type: a type and a subtype,
// lower case, with no parameters.
var mimeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`)

// objects stores each object entry: create_object, then one PUT of the
// file's checked bytes to the upload URL on the compiled file store, and one
// retry through get_object's fresh URL. Each id is saved as soon as Smrit
// gives it, so a resume finishes that object rather than making another.
//
// Objects have no slug, so with no run state nothing tells this CLI a file
// was stored before: a rerun without --resume stores a second object and
// reports it created. That is accepted; the server has no idempotency key
// for objects.
func (r *runner) objects(es []plan.Entry) error {
	for _, e := range es {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if err := r.object(e); err != nil {
			return err
		}
		if err := r.save(); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) object(e plan.Entry) error {
	prev := r.st.Entries[e.Path]
	if state.Done(prev.Status) && prev.ID != "" {
		o := &r.out[e.Index]
		o.Status, o.ID = prev.Status, prev.ID
		if prev.SourceSHA256 != e.SHA256 {
			o.Error = objectChanged
		}
		r.ix.ids[e.Path] = prev.ID
		return nil
	}
	ctype, ok := objectType(e)
	if !ok {
		r.refuse(e, objectBadType)
		return nil
	}
	if e.Size > MaxObjectBytes {
		r.refuse(e, objectTooBig)
		return nil
	}
	data, err := readExact(e)
	if err != nil {
		r.refuse(e, err.Error())
		return nil
	}
	// An object this run made earlier from these same bytes is finished,
	// never made again.
	id, upload := "", ""
	if prev.ID != "" && prev.SourceSHA256 == e.SHA256 {
		id = prev.ID
	} else {
		raw, err := r.o.MCP.Call(r.ctx, "create_object", map[string]any{"name": e.Name, "mime_type": ctype, "scope": e.Scope})
		if err != nil {
			return r.objectRefused(e, "", err)
		}
		var made struct {
			ID        string `json:"id"`
			UploadURL string `json:"upload_url"`
		}
		if json.Unmarshal(raw, &made) != nil || made.ID == "" {
			r.objectFailed(e, objectUnread, "")
			return nil
		}
		id, upload = made.ID, made.UploadURL
		r.objectFailed(e, objectUnfinished, id)
		if err := r.save(); err != nil {
			return err
		}
	}
	reason := ""
	for range 2 {
		if upload == "" {
			fresh, arrived, why, err := r.freshUpload(e, id)
			if err != nil {
				return r.objectRefused(e, id, err)
			}
			if arrived {
				r.objectStored(e, id)
				return nil
			}
			if why != "" {
				reason = why
				break
			}
			upload = fresh
		}
		why, err := r.put(upload, data, ctype)
		upload = ""
		if err != nil {
			return err
		}
		if why == "" {
			r.objectStored(e, id)
			return nil
		}
		reason = why
	}
	r.objectFailed(e, reason, id)
	return nil
}

// objectFailed keeps the id of an object whose bytes Smrit does not hold,
// for a resume to finish it. It is no link target: a document refers only
// to a stored object.
func (r *runner) objectFailed(e plan.Entry, reason, id string) {
	r.record(e, state.EntryState{Kind: e.Kind, SourceSHA256: e.SHA256, Status: state.StatusFailed, ID: id, Error: reason, Phase: r.st.Phase})
}

func (r *runner) objectStored(e plan.Entry, id string) {
	r.ix.ids[e.Path] = id
	r.record(e, state.EntryState{Kind: e.Kind, SourceSHA256: e.SHA256, Status: state.StatusCreated, ID: id, Phase: r.st.Phase})
}

// objectRefused fails the entry on a refusal from the tool; anything else,
// such as Smrit being out of reach, stops the run.
func (r *runner) objectRefused(e plan.Entry, id string, err error) error {
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		return err
	}
	msg := plainSentence(te.Message)
	if msg == "" || strings.Contains(msg, "/u/") {
		msg = "Smrit refused this file."
	}
	r.objectFailed(e, "Smrit refused this file: "+msg, id)
	return nil
}

// objectType is the plan's type for the object when it gave one, or the
// type its extension names, or application/octet-stream. A plan type that
// is not a MIME type is refused rather than guessed past.
func objectType(e plan.Entry) (string, bool) {
	if t := strings.TrimSpace(e.Type); t != "" {
		mt, params, err := mime.ParseMediaType(t)
		if err != nil || len(params) > 0 || !mimeRE.MatchString(mt) {
			return "", false
		}
		return mt, true
	}
	if mt, _, err := mime.ParseMediaType(mime.TypeByExtension(strings.ToLower(path.Ext(e.Path)))); err == nil && mimeRE.MatchString(mt) {
		return mt, true
	}
	return "application/octet-stream", true
}

// freshUpload asks get_object for a new upload URL. arrived means Smrit
// already holds the bytes; why is a sentence when there is no URL to use.
func (r *runner) freshUpload(e plan.Entry, id string) (upload string, arrived bool, why string, err error) {
	raw, err := r.o.MCP.Call(r.ctx, "get_object", map[string]any{"object_id": id})
	if err != nil {
		return "", false, "", err
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return "", false, objectUnread, nil
	}
	// The answer begins with the object's name, which this run chose.
	rest, ok := strings.CutPrefix(text, strings.TrimSpace(e.Name)+" · ")
	if !ok {
		return "", false, objectUnread, nil
	}
	waiting, ok := strings.CutPrefix(rest, "waiting for its bytes")
	if !ok {
		return "", true, "", nil
	}
	const marker = " · send them once with an HTTP PUT to "
	if i := strings.LastIndex(waiting, marker); i >= 0 {
		return strings.TrimSpace(waiting[i+len(marker):]), false, "", nil
	}
	if strings.Contains(waiting, "too late to upload") {
		return "", false, objectTooLate, nil
	}
	return "", false, objectNoURL, nil
}

// troveOrigin is the compiled file store origin.
func (r *runner) troveOrigin() string {
	if r.o.trove != "" {
		return r.o.trove
	}
	return server.TroveOrigin
}

// uploadable reports whether u may be sent bytes: https, or http only in a
// dev build whose compiled origin is http, on exactly the compiled origin's
// scheme and host, with nothing but a path.
func (r *runner) uploadable(u *url.URL) bool {
	origin, err := url.Parse(r.troveOrigin())
	if err != nil || origin.Host == "" {
		return false
	}
	secure := origin.Scheme == "https" || (server.Dev && origin.Scheme == "http" && r.o.trove == "")
	return secure && u.Scheme == origin.Scheme && strings.EqualFold(u.Host, origin.Host) &&
		u.User == nil && u.Opaque == "" && u.Fragment == ""
}

// uploader is an HTTP client that never follows a redirect and keeps no
// cookies.
func (r *runner) uploader() *http.Client {
	c := http.Client{Timeout: uploadTimeout}
	if r.o.uploadHTTP != nil {
		c = *r.o.uploadHTTP
	}
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// put sends data to the upload URL once, with no credential of this CLI's:
// the URL is its own. why is a sentence when the bytes were not stored; err
// is the run being stopped.
func (r *runner) put(upload string, data []byte, ctype string) (why string, err error) {
	u, perr := url.Parse(upload)
	if perr != nil || !r.uploadable(u) {
		return objectBadURL, nil
	}
	req, rerr := http.NewRequestWithContext(r.ctx, http.MethodPut, u.String(), bytes.NewReader(data))
	if rerr != nil {
		return objectBadURL, nil
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", ctype)
	resp, derr := r.uploader().Do(req)
	if derr != nil {
		// The error names the URL, so it goes no further than here.
		if cerr := r.ctx.Err(); cerr != nil {
			return "", cerr
		}
		return objectNotSent, nil
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Sprintf("Smrit's file store answered %d, so the file was not stored.", resp.StatusCode), nil
	}
	return "", nil
}
