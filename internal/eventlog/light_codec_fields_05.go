package eventlog

import (
	"encoding/json"
	"github.com/majorcontext/harness/protocol"
	"sort"
	"time"
)

var _ json.RawMessage
var _ time.Time
var _ = sort.Strings
var _ protocol.Settings

func codecMapOfJSONRawMessageEncode(w *codecWriter, v map[string]json.RawMessage) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	for _, k := range keys {
		w.string(k)
		codecJSONRawMessageEncode(w, v[string(k)])
	}
}
func codecMapOfJSONRawMessageDecode(r *codecReader, v *map[string]json.RawMessage) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make(map[string]json.RawMessage, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x json.RawMessage
		codecJSONRawMessageDecode(r, &x)
		(*v)[k] = x
	}
}

func codecSliceOfProtocolCitationEncode(w *codecWriter, v []protocol.Citation) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolCitationEncode(w, x)
	}
}
func codecSliceOfProtocolCitationDecode(r *codecReader, v *[]protocol.Citation) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]protocol.Citation, n)
	for i := range *v {
		codecProtocolCitationDecode(r, &(*v)[i])
	}
}

func codecSliceOfProtocolSearchResultEncode(w *codecWriter, v []protocol.SearchResult) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolSearchResultEncode(w, x)
	}
}
func codecSliceOfProtocolSearchResultDecode(r *codecReader, v *[]protocol.SearchResult) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]protocol.SearchResult, n)
	for i := range *v {
		codecProtocolSearchResultDecode(r, &(*v)[i])
	}
}

func codecProtocolCitationEncode(w *codecWriter, v protocol.Citation) {
	codecStringEncode(w, v.URL)
	codecStringEncode(w, v.Title)
	codecStringEncode(w, v.Quote)
}
func codecProtocolCitationDecode(r *codecReader, v *protocol.Citation) {
	codecStringDecode(r, &v.URL)
	codecStringDecode(r, &v.Title)
	codecStringDecode(r, &v.Quote)
}

func codecProtocolSearchResultEncode(w *codecWriter, v protocol.SearchResult) {
	codecStringEncode(w, v.Title)
	codecStringEncode(w, v.URL)
}
func codecProtocolSearchResultDecode(r *codecReader, v *protocol.SearchResult) {
	codecStringDecode(r, &v.Title)
	codecStringDecode(r, &v.URL)
}
