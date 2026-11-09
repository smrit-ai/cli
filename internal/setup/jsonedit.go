package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNotJSON means a settings file does not parse, so it is left alone.
var ErrNotJSON = errors.New("not valid JSON")

// node is one JSON value with its object members in file order. Numbers keep
// their literal text, so 1.50 stays 1.50.
type node struct {
	kind  byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'z' null
	keys  []string
	vals  []*node
	items []*node
	str   string
	num   json.Number
	b     bool
}

func parseJSON(raw []byte) (*node, error) {
	if !json.Valid(raw) {
		return nil, ErrNotJSON
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := parseValue(dec)
	if err != nil {
		return nil, ErrNotJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrNotJSON
	}
	return n, nil
}

func parseValue(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &node{kind: 'o'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, ErrNotJSON
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k)
				n.vals = append(n.vals, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '[':
			n := &node{kind: 'a'}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
	case string:
		return &node{kind: 's', str: t}, nil
	case json.Number:
		return &node{kind: 'n', num: t}, nil
	case bool:
		return &node{kind: 'b', b: t}, nil
	case nil:
		return &node{kind: 'z'}, nil
	}
	return nil, ErrNotJSON
}

func (n *node) get(key string) *node {
	for i, k := range n.keys {
		if k == key {
			return n.vals[i]
		}
	}
	return nil
}

func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

func (n *node) write(b *strings.Builder, indent string) {
	switch n.kind {
	case 'o':
		if len(n.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range n.keys {
			b.WriteString(indent + "  " + quote(k) + ": ")
			n.vals[i].write(b, indent+"  ")
			if i < len(n.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case 'a':
		if len(n.items) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, v := range n.items {
			b.WriteString(indent + "  ")
			v.write(b, indent+"  ")
			if i < len(n.items)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	case 's':
		b.WriteString(quote(n.str))
	case 'n':
		b.WriteString(n.num.String())
	case 'b':
		if n.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	default:
		b.WriteString("null")
	}
}

// AddToList adds value to the list of strings at path in a JSON object,
// creating the objects and the list on the way when they are missing. Every
// other key keeps its place and its value; the result is indented by two
// spaces and ends with a newline. When the value is in the list already,
// raw comes back unchanged and changed is false. An empty file counts as an
// empty object; anything else that does not parse is ErrNotJSON.
func AddToList(raw []byte, path []string, value string) (out []byte, changed bool, err error) {
	root := &node{kind: 'o'}
	if len(bytes.TrimSpace(raw)) > 0 {
		if root, err = parseJSON(raw); err != nil {
			return nil, false, err
		}
		if root.kind != 'o' {
			return nil, false, errors.New("it is JSON but not an object")
		}
	}
	cur := root
	for i, key := range path {
		next := cur.get(key)
		last := i == len(path)-1
		if next == nil {
			if last {
				next = &node{kind: 'a'}
			} else {
				next = &node{kind: 'o'}
			}
			cur.keys = append(cur.keys, key)
			cur.vals = append(cur.vals, next)
		}
		want := byte('o')
		if last {
			want = 'a'
		}
		if next.kind != want {
			kind := "an object"
			if last {
				kind = "a list"
			}
			return nil, false, fmt.Errorf("%s in it is not %s", strings.Join(path[:i+1], "."), kind)
		}
		cur = next
	}
	for _, it := range cur.items {
		if it.kind == 's' && it.str == value {
			return raw, false, nil
		}
	}
	cur.items = append(cur.items, &node{kind: 's', str: value})
	var b strings.Builder
	root.write(&b, "")
	b.WriteByte('\n')
	return []byte(b.String()), true, nil
}
