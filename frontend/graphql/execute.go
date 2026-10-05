package graphql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/fpt/go-dquery/exec"
	"github.com/fpt/go-dquery/opt"
	"github.com/fpt/go-dquery/value"
)

// Response is a GraphQL response. Data is omitted when the request failed
// before execution, and null when execution failed.
type Response struct {
	Data   *Object       `json:"data"`
	Errors gqlerror.List `json:"errors,omitempty"`
}

// Object is a JSON object that preserves key order, as GraphQL requires.
type Object struct {
	Keys   []string
	Values []any
}

func (o *Object) Set(k string, v any) {
	o.Keys = append(o.Keys, k)
	o.Values = append(o.Values, v)
}

// Get returns the value for k, or nil.
func (o *Object) Get(k string) any {
	for i, key := range o.Keys {
		if key == k {
			return o.Values[i]
		}
	}
	return nil
}

func (o *Object) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("null"), nil
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.Keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(o.Values[i])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Execute compiles and runs req. Root fields run in order; for mutations
// each root field commits on its own, and execution stops at the first
// error (earlier fields stay committed).
func (s *Schema) Execute(ctx context.Context, ex *exec.Executor, req Request, o opt.Options) *Response {
	op, errs := s.Compile(req, o)
	if len(errs) > 0 {
		return &Response{Errors: errs}
	}
	data := &Object{}
	for _, f := range op.Fields {
		v, err := f.run(ctx, ex)
		if err != nil {
			return &Response{Errors: gqlerror.List{gqlerror.WrapPath(ast.Path{ast.PathName(f.Key)}, err)}}
		}
		data.Set(f.Key, v)
	}
	return &Response{Data: data}
}

func (f *RootField) run(ctx context.Context, ex *exec.Executor) (any, error) {
	if f.kind == typenameKind {
		return f.name, nil
	}
	res, err := ex.Execute(ctx, f.Plan)
	if err != nil {
		return nil, err
	}
	switch f.kind {
	case rootList:
		out := make([]any, len(res.Rows))
		for i, r := range res.Rows {
			out[i] = rowObject(res.Columns, r)
		}
		return out, nil
	case rootByKey:
		if len(res.Rows) == 0 {
			return nil, nil
		}
		return rowObject(res.Columns, res.Rows[0]), nil
	}
	obj := &Object{}
	for _, it := range f.resp {
		switch it.what {
		case "affected_rows":
			obj.Set(it.key, res.RowsAffected)
		case "returning":
			list := make([]any, len(res.Rows))
			for i, r := range res.Rows {
				list[i] = rowObject(res.Columns, r)
			}
			obj.Set(it.key, list)
		case "__typename":
			obj.Set(it.key, it.name)
		}
	}
	return obj, nil
}

func rowObject(cols []string, row value.Row) *Object {
	o := &Object{Keys: cols, Values: make([]any, len(row))}
	for i, v := range row {
		o.Values[i] = jsonValue(v)
	}
	return o
}

func jsonValue(v value.Value) any {
	switch v.Kind() {
	case value.KindNull:
		return nil
	case value.KindBool:
		return v.Bool()
	case value.KindInt:
		return v.Int()
	case value.KindFloat:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil // not representable in JSON
		}
		return f
	case value.KindString:
		return v.Str()
	case value.KindBytes:
		return base64.StdEncoding.EncodeToString(v.Bytes())
	case value.KindTimestamp:
		return v.Time().Format(time.RFC3339Nano)
	case value.KindList:
		out := make([]any, len(v.List()))
		for i, e := range v.List() {
			out[i] = jsonValue(e)
		}
		return out
	case value.KindRecord:
		r := v.Record()
		o := &Object{Keys: r.Names, Values: make([]any, len(r.Values))}
		for i, e := range r.Values {
			o.Values[i] = jsonValue(e)
		}
		return o
	}
	return nil
}

// Handler serves GraphQL over HTTP: POST with a JSON body
// {"query", "operationName", "variables"}, or GET with ?query=. GET only
// runs queries. A GET on the "sdl" sub-path returns the schema.
func (s *Schema) Handler(ex *exec.Executor, o opt.Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		switch r.Method {
		case http.MethodGet:
			req.Query = r.URL.Query().Get("query")
			req.OperationName = r.URL.Query().Get("operationName")
			if v := r.URL.Query().Get("variables"); v != "" {
				if err := json.Unmarshal([]byte(v), &req.Variables); err != nil {
					http.Error(w, "bad variables: "+err.Error(), http.StatusBadRequest)
					return
				}
			}
		case http.MethodPost:
			dec := json.NewDecoder(r.Body)
			dec.UseNumber()
			if err := dec.Decode(&req); err != nil {
				http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
				return
			}
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodGet {
			if op, errs := s.Compile(req, o); len(errs) == 0 && op.Mutation {
				http.Error(w, "mutations require POST", http.StatusMethodNotAllowed)
				return
			}
		}
		resp := s.Execute(r.Context(), ex, req, o)
		w.Header().Set("Content-Type", "application/json")
		if resp.Data == nil {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(resp)
	})
}

// SDLHandler serves the schema in SDL.
func (s *Schema) SDLHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(s.sdl))
	})
}
