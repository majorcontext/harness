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

func codecProtocolStatusEncode(w *codecWriter, v protocol.Status) {
	w.string(string(v))
}
func codecProtocolStatusDecode(r *codecReader, v *protocol.Status) {
	*v = protocol.Status(r.string())
}

func codecSliceOfstring41Encode(w *codecWriter, v []string) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecStringEncode(w, x)
	}
}
func codecSliceOfstring41Decode(r *codecReader, v *[]string) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(*new(string))) {
		return
	}
	*v = make([]string, n)
	for i := range *v {
		codecStringDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecSliceOfstring42Encode(w *codecWriter, v []string) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecStringEncode(w, x)
	}
}
func codecSliceOfstring42Decode(r *codecReader, v *[]string) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(*new(string))) {
		return
	}
	*v = make([]string, n)
	for i := range *v {
		codecStringDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecPtrToProtocolGoalViewEncode(w *codecWriter, v *protocol.GoalView) {
	w.boolean(v != nil)
	if v != nil {
		codecProtocolGoalViewEncode(w, *v)
	}
}
func codecPtrToProtocolGoalViewDecode(r *codecReader, v **protocol.GoalView) {
	if r.boolean() {
		if !r.allocate(1, unsafe.Sizeof(*new(protocol.GoalView))) {
			return
		}
		v2 := new(protocol.GoalView)
		codecProtocolGoalViewDecode(r, v2)
		*v = v2
	} else {
		*v = nil
	}
}

func codecPtrToProtocolLastTurnEncode(w *codecWriter, v *protocol.LastTurn) {
	w.boolean(v != nil)
	if v != nil {
		codecProtocolLastTurnEncode(w, *v)
	}
}
func codecPtrToProtocolLastTurnDecode(r *codecReader, v **protocol.LastTurn) {
	if r.boolean() {
		if !r.allocate(1, unsafe.Sizeof(*new(protocol.LastTurn))) {
			return
		}
		v2 := new(protocol.LastTurn)
		codecProtocolLastTurnDecode(r, v2)
		*v = v2
	} else {
		*v = nil
	}
}

func codecSliceOfstring45Encode(w *codecWriter, v []string) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecStringEncode(w, x)
	}
}
func codecSliceOfstring45Decode(r *codecReader, v *[]string) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(*new(string))) {
		return
	}
	*v = make([]string, n)
	for i := range *v {
		codecStringDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecProtocolStopReasonEncode(w *codecWriter, v protocol.StopReason) {
	w.string(string(v))
}
func codecProtocolStopReasonDecode(r *codecReader, v *protocol.StopReason) {
	*v = protocol.StopReason(r.string())
}

func codecProtocolCauseEncode(w *codecWriter, v protocol.Cause) {
	w.string(string(v))
}
func codecProtocolCauseDecode(r *codecReader, v *protocol.Cause) {
	*v = protocol.Cause(r.string())
}

func codecProtocolErrorClassEncode(w *codecWriter, v protocol.ErrorClass) {
	w.string(string(v))
}
func codecProtocolErrorClassDecode(r *codecReader, v *protocol.ErrorClass) {
	*v = protocol.ErrorClass(r.string())
}

func codecInt64Encode(w *codecWriter, v int64) {
	w.signed(int64(v))
}
func codecInt64Decode(r *codecReader, v *int64) {
	*v = int64(r.signed())
}

func codecOpenToolCallEncode(w *codecWriter, v OpenToolCall) {
	codecStringEncode(w, v.CallID)
	codecStringEncode(w, v.ItemID)
	codecStringEncode(w, v.Name)
	codecBoolEncode(w, v.answered)
}
func codecOpenToolCallDecode(r *codecReader, v *OpenToolCall) {
	codecStringDecode(r, &v.CallID)
	codecStringDecode(r, &v.ItemID)
	codecStringDecode(r, &v.Name)
	codecBoolDecode(r, &v.answered)
}

func codecPendingRequestEncode(w *codecWriter, v pendingRequest) {
	codecProtocolRequestOpenedEncode(w, v.RequestOpened)
	codecStringEncode(w, v.turnID)
}
func codecPendingRequestDecode(r *codecReader, v *pendingRequest) {
	codecProtocolRequestOpenedDecode(r, &v.RequestOpened)
	codecStringDecode(r, &v.turnID)
}

func codecProtocolRequestResolvedEncode(w *codecWriter, v protocol.RequestResolved) {
	codecStringEncode(w, v.RequestID)
	codecProtocolResolutionKindEncode(w, v.Resolution)
	codecJSONRawMessageEncode(w, v.Answer)
	codecStringEncode(w, v.Response)
	codecStringEncode(w, v.Source)
	codecStringEncode(w, v.SourceID)
	codecStringEncode(w, v.SourceLabel)
}
func codecProtocolRequestResolvedDecode(r *codecReader, v *protocol.RequestResolved) {
	codecStringDecode(r, &v.RequestID)
	codecProtocolResolutionKindDecode(r, &v.Resolution)
	codecJSONRawMessageDecode(r, &v.Answer)
	codecStringDecode(r, &v.Response)
	codecStringDecode(r, &v.Source)
	codecStringDecode(r, &v.SourceID)
	codecStringDecode(r, &v.SourceLabel)
}

func codecJSONRawMessageEncode(w *codecWriter, v json.RawMessage) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecByteEncode(w, x)
	}
}
func codecJSONRawMessageDecode(r *codecReader, v *json.RawMessage) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, 1) {
		return
	}
	*v = make(json.RawMessage, n)
	for i := range *v {
		codecByteDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecPtrToProtocolSubscriptionUsage54Encode(w *codecWriter, v *protocol.SubscriptionUsage) {
	w.boolean(v != nil)
	if v != nil {
		codecProtocolSubscriptionUsageEncode(w, *v)
	}
}
func codecPtrToProtocolSubscriptionUsage54Decode(r *codecReader, v **protocol.SubscriptionUsage) {
	if r.boolean() {
		if !r.allocate(1, unsafe.Sizeof(*new(protocol.SubscriptionUsage))) {
			return
		}
		v2 := new(protocol.SubscriptionUsage)
		codecProtocolSubscriptionUsageDecode(r, v2)
		*v = v2
	} else {
		*v = nil
	}
}

func codecPtrTofloat6455Encode(w *codecWriter, v *float64) {
	w.boolean(v != nil)
	if v != nil {
		codecFloat64Encode(w, *v)
	}
}
func codecPtrTofloat6455Decode(r *codecReader, v **float64) {
	if r.boolean() {
		if !r.allocate(1, unsafe.Sizeof(*new(float64))) {
			return
		}
		v2 := new(float64)
		codecFloat64Decode(r, v2)
		*v = v2
	} else {
		*v = nil
	}
}

func codecProtocolSubscriptionUsageEncode(w *codecWriter, v protocol.SubscriptionUsage) {
	codecStringEncode(w, v.Provider)
	codecStringEncode(w, v.Account)
	codecStringEncode(w, v.Plan)
	codecSliceOfProtocolSubscriptionUsageWindowEncode(w, v.Windows)
	codecPtrToProtocolSubscriptionOverageEncode(w, v.Overage)
	codecInt64Encode(w, v.CapturedAt)
	codecPtrTofloat64Encode(w, v.SessionCostUSD)
}
func codecProtocolSubscriptionUsageDecode(r *codecReader, v *protocol.SubscriptionUsage) {
	codecStringDecode(r, &v.Provider)
	codecStringDecode(r, &v.Account)
	codecStringDecode(r, &v.Plan)
	codecSliceOfProtocolSubscriptionUsageWindowDecode(r, &v.Windows)
	codecPtrToProtocolSubscriptionOverageDecode(r, &v.Overage)
	codecInt64Decode(r, &v.CapturedAt)
	codecPtrTofloat64Decode(r, &v.SessionCostUSD)
}

func codecFloat64Encode(w *codecWriter, v float64) {
	w.float64(float64(v))
}
func codecFloat64Decode(r *codecReader, v *float64) {
	*v = float64(r.float64())
}

func codecProtocolOutcomeEncode(w *codecWriter, v protocol.Outcome) {
	w.string(string(v))
}
func codecProtocolOutcomeDecode(r *codecReader, v *protocol.Outcome) {
	*v = protocol.Outcome(r.string())
}

func codecBackendChainEncode(w *codecWriter, v BackendChain) {
	codecJSONRawMessageEncode(w, v.Head)
	codecSliceOfstringEncode(w, v.Chunks)
	codecStringEncode(w, v.Legacy)
	codecIntEncode(w, v.Entries)
	codecStringEncode(w, v.Sum)
}
func codecBackendChainDecode(r *codecReader, v *BackendChain) {
	codecJSONRawMessageDecode(r, &v.Head)
	codecSliceOfstringDecode(r, &v.Chunks)
	codecStringDecode(r, &v.Legacy)
	codecIntDecode(r, &v.Entries)
	codecStringDecode(r, &v.Sum)
}
