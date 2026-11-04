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

func codecSliceOfProtocolInputAdmittedEncode(w *codecWriter, v []protocol.InputAdmitted) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolInputAdmittedEncode(w, x)
	}
}
func codecSliceOfProtocolInputAdmittedDecode(r *codecReader, v *[]protocol.InputAdmitted) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]protocol.InputAdmitted, n)
	for i := range *v {
		codecProtocolInputAdmittedDecode(r, &(*v)[i])
	}
}

func codecProvenanceEncode(w *codecWriter, v provenance) {
	codecStringEncode(w, v.source)
	codecStringEncode(w, v.id)
	codecStringEncode(w, v.label)
}
func codecProvenanceDecode(r *codecReader, v *provenance) {
	codecStringDecode(r, &v.source)
	codecStringDecode(r, &v.id)
	codecStringDecode(r, &v.label)
}

func codecSliceOfProtocolMessagePartEncode(w *codecWriter, v []protocol.MessagePart) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolMessagePartEncode(w, x)
	}
}
func codecSliceOfProtocolMessagePartDecode(r *codecReader, v *[]protocol.MessagePart) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]protocol.MessagePart, n)
	for i := range *v {
		codecProtocolMessagePartDecode(r, &(*v)[i])
	}
}

func codecSliceOfProtocolOperatorBatchEntryEncode(w *codecWriter, v []protocol.OperatorBatchEntry) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolOperatorBatchEntryEncode(w, x)
	}
}
func codecSliceOfProtocolOperatorBatchEntryDecode(r *codecReader, v *[]protocol.OperatorBatchEntry) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]protocol.OperatorBatchEntry, n)
	for i := range *v {
		codecProtocolOperatorBatchEntryDecode(r, &(*v)[i])
	}
}

func codecProtocolDeliveryEncode(w *codecWriter, v protocol.Delivery) {
	w.string(string(v))
}
func codecProtocolDeliveryDecode(r *codecReader, v *protocol.Delivery) {
	*v = protocol.Delivery(r.string())
}

func codecSliceOfProtocolLogPartEncode(w *codecWriter, v []protocol.LogPart) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolLogPartEncode(w, x)
	}
}
func codecSliceOfProtocolLogPartDecode(r *codecReader, v *[]protocol.LogPart) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]protocol.LogPart, n)
	for i := range *v {
		codecProtocolLogPartDecode(r, &(*v)[i])
	}
}

func codecProtocolGoalEncode(w *codecWriter, v protocol.Goal) {
	codecStringEncode(w, v.Condition)
	codecIntEncode(w, v.MaxTurns)
}
func codecProtocolGoalDecode(r *codecReader, v *protocol.Goal) {
	codecStringDecode(r, &v.Condition)
	codecIntDecode(r, &v.MaxTurns)
}

func codecProtocolGoalStateEncode(w *codecWriter, v protocol.GoalState) {
	w.string(string(v))
}
func codecProtocolGoalStateDecode(r *codecReader, v *protocol.GoalState) {
	*v = protocol.GoalState(r.string())
}

func codecProtocolVerdictEncode(w *codecWriter, v protocol.Verdict) {
	w.string(string(v))
}
func codecProtocolVerdictDecode(r *codecReader, v *protocol.Verdict) {
	*v = protocol.Verdict(r.string())
}

func codecProtocolSubscriptionUsageWindowEncode(w *codecWriter, v protocol.SubscriptionUsageWindow) {
	codecStringEncode(w, v.Key)
	codecStringEncode(w, v.Label)
	codecFloat64Encode(w, v.UsedPercent)
	codecInt64Encode(w, v.ResetsAt)
}
func codecProtocolSubscriptionUsageWindowDecode(r *codecReader, v *protocol.SubscriptionUsageWindow) {
	codecStringDecode(r, &v.Key)
	codecStringDecode(r, &v.Label)
	codecFloat64Decode(r, &v.UsedPercent)
	codecInt64Decode(r, &v.ResetsAt)
}

func codecProtocolSubscriptionOverageEncode(w *codecWriter, v protocol.SubscriptionOverage) {
	codecBoolEncode(w, v.InUse)
	codecStringEncode(w, v.Status)
	codecInt64Encode(w, v.ResetsAt)
}
func codecProtocolSubscriptionOverageDecode(r *codecReader, v *protocol.SubscriptionOverage) {
	codecBoolDecode(r, &v.InUse)
	codecStringDecode(r, &v.Status)
	codecInt64Decode(r, &v.ResetsAt)
}

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
	*v = make(map[string]any, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x any
		codecAnyDecode(r, &x)
		(*v)[k] = x
	}
}

func codecSliceOfProtocolLogPart92Encode(w *codecWriter, v []protocol.LogPart) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolLogPartEncode(w, x)
	}
}
func codecSliceOfProtocolLogPart92Decode(r *codecReader, v *[]protocol.LogPart) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]protocol.LogPart, n)
	for i := range *v {
		codecProtocolLogPartDecode(r, &(*v)[i])
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
	codecSliceOfProtocolCitation97Encode(w, v.Citations)
	codecStringEncode(w, v.Query)
	codecSliceOfProtocolSearchResult98Encode(w, v.Results)
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
	codecSliceOfProtocolCitation97Decode(r, &v.Citations)
	codecStringDecode(r, &v.Query)
	codecSliceOfProtocolSearchResult98Decode(r, &v.Results)
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
	codecSliceOfProtocolLogPart99Encode(w, v.Blobs)
	codecMapOfJSONRawMessageEncode(w, v.ProviderData)
	codecSliceOfProtocolCitationEncode(w, v.Citations)
	codecStringEncode(w, v.Query)
	codecSliceOfProtocolSearchResultEncode(w, v.Results)
}
func codecProtocolLogPartDecode(r *codecReader, v *protocol.LogPart) {
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
	codecSliceOfProtocolLogPart99Decode(r, &v.Blobs)
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

func codecSliceOfProtocolCitation97Encode(w *codecWriter, v []protocol.Citation) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolCitationEncode(w, x)
	}
}
func codecSliceOfProtocolCitation97Decode(r *codecReader, v *[]protocol.Citation) {
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

func codecSliceOfProtocolSearchResult98Encode(w *codecWriter, v []protocol.SearchResult) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolSearchResultEncode(w, x)
	}
}
func codecSliceOfProtocolSearchResult98Decode(r *codecReader, v *[]protocol.SearchResult) {
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

func codecSliceOfProtocolLogPart99Encode(w *codecWriter, v []protocol.LogPart) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolLogPartEncode(w, x)
	}
}
func codecSliceOfProtocolLogPart99Decode(r *codecReader, v *[]protocol.LogPart) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]protocol.LogPart, n)
	for i := range *v {
		codecProtocolLogPartDecode(r, &(*v)[i])
	}
}
