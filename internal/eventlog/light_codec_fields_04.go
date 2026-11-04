package eventlog

import (
	"encoding/json"
	"github.com/majorcontext/harness/protocol"
	"sort"
	"time"
	"unsafe"
)

var _ json.RawMessage
var _ time.Time
var _ = sort.Strings
var _ protocol.Settings
var _ = unsafe.Sizeof(0)

func codecMapOfanyEncode(w *codecWriter, v map[string]any) {
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
		codecAnyEncode(w, v[string(k)])
	}
}
func codecMapOfanyDecode(r *codecReader, v *map[string]any) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(any))) {
		return
	}
	*v = make(map[string]any, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x any
		codecAnyDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecProtocolMessagePartEncode(w *codecWriter, v protocol.MessagePart) {
	codecStringEncode(w, v.Type)
	codecStringEncode(w, v.Text)
	codecStringEncode(w, v.CallID)
	codecStringEncode(w, v.Name)
	codecJSONRawMessageEncode(w, v.Arguments)
	codecStringEncode(w, v.Content)
	codecBoolEncode(w, v.IsError)
	codecStringEncode(w, v.MediaType)
	codecIntEncode(w, v.Bytes)
	codecStringEncode(w, v.Key)
	codecSliceOfProtocolCitationEncode(w, v.Citations)
	codecStringEncode(w, v.Query)
	codecSliceOfProtocolSearchResultEncode(w, v.Results)
}
func codecProtocolMessagePartDecode(r *codecReader, v *protocol.MessagePart) {
	codecStringDecode(r, &v.Type)
	codecStringDecode(r, &v.Text)
	codecStringDecode(r, &v.CallID)
	codecStringDecode(r, &v.Name)
	codecJSONRawMessageDecode(r, &v.Arguments)
	codecStringDecode(r, &v.Content)
	codecBoolDecode(r, &v.IsError)
	codecStringDecode(r, &v.MediaType)
	codecIntDecode(r, &v.Bytes)
	codecStringDecode(r, &v.Key)
	codecSliceOfProtocolCitationDecode(r, &v.Citations)
	codecStringDecode(r, &v.Query)
	codecSliceOfProtocolSearchResultDecode(r, &v.Results)
}

func codecProtocolOperatorBatchEntryEncode(w *codecWriter, v protocol.OperatorBatchEntry) {
	codecStringEncode(w, v.ID)
	codecStringEncode(w, v.Text)
	codecStringEncode(w, v.Source)
	codecStringEncode(w, v.SourceID)
	codecStringEncode(w, v.SourceLabel)
}
func codecProtocolOperatorBatchEntryDecode(r *codecReader, v *protocol.OperatorBatchEntry) {
	codecStringDecode(r, &v.ID)
	codecStringDecode(r, &v.Text)
	codecStringDecode(r, &v.Source)
	codecStringDecode(r, &v.SourceID)
	codecStringDecode(r, &v.SourceLabel)
}

func codecProtocolLogPartEncode(w *codecWriter, v protocol.LogPart) {
	codecStringEncode(w, v.Type)
	codecStringEncode(w, v.Text)
	codecStringEncode(w, v.CallID)
	codecStringEncode(w, v.Name)
	codecJSONRawMessageEncode(w, v.Arguments)
	codecStringEncode(w, v.ArgumentsText)
	codecBoolEncode(w, v.IsError)
	codecStringEncode(w, v.MediaType)
	codecStringEncode(w, v.BlobKey)
	codecIntEncode(w, v.Bytes)
	codecSliceOfProtocolLogPartEncode(w, v.Blobs)
	codecMapOfJSONRawMessageEncode(w, v.ProviderData)
	codecSliceOfProtocolCitationEncode(w, v.Citations)
	codecStringEncode(w, v.Query)
	codecSliceOfProtocolSearchResultEncode(w, v.Results)
}
func codecProtocolLogPartDecode(r *codecReader, v *protocol.LogPart) {
	if !r.enter() {
		return
	}
	defer r.leave()
	codecStringDecode(r, &v.Type)
	codecStringDecode(r, &v.Text)
	codecStringDecode(r, &v.CallID)
	codecStringDecode(r, &v.Name)
	codecJSONRawMessageDecode(r, &v.Arguments)
	codecStringDecode(r, &v.ArgumentsText)
	codecBoolDecode(r, &v.IsError)
	codecStringDecode(r, &v.MediaType)
	codecStringDecode(r, &v.BlobKey)
	codecIntDecode(r, &v.Bytes)
	codecSliceOfProtocolLogPartDecode(r, &v.Blobs)
	codecMapOfJSONRawMessageDecode(r, &v.ProviderData)
	codecSliceOfProtocolCitationDecode(r, &v.Citations)
	codecStringDecode(r, &v.Query)
	codecSliceOfProtocolSearchResultDecode(r, &v.Results)
}

func codecAnyEncode(w *codecWriter, v any) {
	w.value(v)
}
func codecAnyDecode(r *codecReader, v *any) {
	*v = r.value()
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
	if !r.allocate(n, unsafe.Sizeof(protocol.Citation{})) {
		return
	}
	*v = make([]protocol.Citation, n)
	for i := range *v {
		codecProtocolCitationDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
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
	if !r.allocate(n, unsafe.Sizeof(protocol.SearchResult{})) {
		return
	}
	*v = make([]protocol.SearchResult, n)
	for i := range *v {
		codecProtocolSearchResultDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
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
