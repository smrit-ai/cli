package setup

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// harnessesJSON is a copy of the server's harness catalogue,
// api/smrit/agent_setup/harnesses.json, synced by make sync-catalog.
//
//go:embed harnesses.json
var harnessesJSON []byte

// Step is one connect or instruction step of a catalogue route.
type Step struct {
	Title string  `json:"title"`
	Text  string  `json:"text"`
	Value *string `json:"value"`
}

// Route is one way a harness connects, such as desktop or web.
type Route struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Scope        string `json:"scope"`
	Connect      []Step `json:"connect"`
	Instructions []Step `json:"instructions"`
	Sources      []struct {
		Label string `json:"label"`
		URL   string `json:"url"`
	} `json:"sources"`
}

// Harness is one catalogue entry.
type Harness struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Routes      []Route `json:"routes"`
}

// Catalogue is every harness the server's connect page lists.
func Catalogue() ([]Harness, error) {
	var hs []Harness
	if err := json.Unmarshal(harnessesJSON, &hs); err != nil {
		return nil, fmt.Errorf("The built in harness catalogue does not parse: %w", err)
	}
	return hs, nil
}

// Find returns the catalogue entry for id.
func Find(id string) (Harness, bool) {
	hs, err := Catalogue()
	if err != nil {
		return Harness{}, false
	}
	for _, h := range hs {
		if h.ID == id {
			return h, true
		}
	}
	return Harness{}, false
}

var placeholder = regexp.MustCompile(`\{\{([a-z_]+)\}\}`)

// render fills each {{name}} once, as the server does, so a value can never
// introduce another placeholder. An unknown name is left as it is.
func render(s string, values map[string]string) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := values[m[2:len(m)-2]]; ok {
			return v
		}
		return m
	})
}

// Rendered is a harness with every placeholder filled.
func (h Harness) Rendered(values map[string]string) Harness {
	out := h
	out.Routes = nil
	steps := func(in []Step) []Step {
		var o []Step
		for _, s := range in {
			r := Step{Title: render(s.Title, values), Text: render(s.Text, values)}
			if s.Value != nil {
				v := render(*s.Value, values)
				r.Value = &v
			}
			o = append(o, r)
		}
		return o
	}
	for _, r := range h.Routes {
		r.Connect = steps(r.Connect)
		r.Instructions = steps(r.Instructions)
		out.Routes = append(out.Routes, r)
	}
	return out
}

// String is the connect steps as text for a person to follow.
func (h Harness) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: setup cannot change this harness for you. These are its steps.\n", h.Name)
	for _, r := range h.Routes {
		fmt.Fprintf(&b, "\n%s (%s)\n", r.Label, r.Scope)
		n := 0
		for _, part := range []struct {
			name  string
			steps []Step
		}{{"Connect", r.Connect}, {"Instructions", r.Instructions}} {
			if len(part.steps) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n%s\n", part.name)
			for _, s := range part.steps {
				n++
				fmt.Fprintf(&b, "%d. %s\n   %s\n", n, s.Title, s.Text)
				if s.Value != nil && *s.Value != "" {
					b.WriteString("\n")
					for _, l := range strings.Split(*s.Value, "\n") {
						if l == "" {
							b.WriteString("\n")
							continue
						}
						fmt.Fprintf(&b, "   %s\n", l)
					}
					b.WriteString("\n")
				}
			}
		}
		if len(r.Sources) > 0 {
			b.WriteString("\nSources\n")
			for _, s := range r.Sources {
				fmt.Fprintf(&b, "   %s: %s\n", s.Label, s.URL)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
