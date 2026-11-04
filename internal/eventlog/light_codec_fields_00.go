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

func codecStateEncode(w *codecWriter, v State) {
	codecBoolEncode(w, v.created)
	codecStringEncode(w, v.parentID)
	codecStringEncode(w, v.agent)
	codecStringEncode(w, v.origin)
	codecStringEncode(w, v.workDir)
	codecStringEncode(w, v.model)
	codecStringEncode(w, v.renamed)
	codecStringEncode(w, v.modeled)
	codecStringEncode(w, v.derived)
	codecProtocolSettingsEncode(w, v.settings)
	codecSliceOfstringEncode(w, v.allowed)
	codecIntEncode(w, v.maxSteps)
	codecTimeTimeEncode(w, v.createdAt)
	codecTimeTimeEncode(w, v.updatedAt)
	codecMapOfinputEncode(w, v.inputs)
	codecProtocolProgressEncode(w, v.prog)
	codecTurnEncode(w, v.turn)
	codecMapOfboolEncode(w, v.turnIDs)
	codecProtocolTurnEndedEncode(w, v.lastEnded)
	codecSliceOfOpenToolCallEncode(w, v.calls)
	codecMapOfpendingRequestEncode(w, v.requests)
	codecMapOfProtocolRequestResolvedEncode(w, v.resolved)
	codecMapOfJSONRawMessageEncode(w, v.asked)
	codecStringEncode(w, v.evaluated)
	codecProtocolUsageEncode(w, v.usage)
	codecProtocolContextMeasuredEncode(w, v.context)
	codecIntEncode(w, v.measuredTurn)
	codecTimeTimeEncode(w, v.readAt)
	codecTimeDurationEncode(w, v.readTTL)
	codecPtrToProtocolSubscriptionUsageEncode(w, v.subscribed)
	codecPtrTofloat64Encode(w, v.cost)
	codecProtocolCompactionAppliedEncode(w, v.compaction)
	codecUint64Encode(w, v.compactedAt)
	codecTimeTimeEncode(w, v.compactedTime)
	codecIntEncode(w, v.compacted)
	codecIntEncode(w, v.compactedLen)
	codecMapOfProtocolOutcomeEncode(w, v.children)
	codecMapOfBackendChainEncode(w, v.backends)
	codecSliceOfProtocolToolResultRetainedEncode(w, v.retained)
	codecMapOfcommandEncode(w, v.commands)
	codecSliceOfentryEncode(w, v.history)
	codecSliceOfProtocolMessageEncode(w, v.folded)
	codecSliceOfentryEncode(w, v.stranded)
	codecIntEncode(w, v.turnAt)
	codecStringEncode(w, v.turnBy)
	codecIntEncode(w, v.turnN)
	codecIntEncode(w, v.turnItems)
	codecSliceOfintEncode(w, v.unran)
}
func codecStateDecode(r *codecReader, v *State) {
	codecBoolDecode(r, &v.created)
	codecStringDecode(r, &v.parentID)
	codecStringDecode(r, &v.agent)
	codecStringDecode(r, &v.origin)
	codecStringDecode(r, &v.workDir)
	codecStringDecode(r, &v.model)
	codecStringDecode(r, &v.renamed)
	codecStringDecode(r, &v.modeled)
	codecStringDecode(r, &v.derived)
	codecProtocolSettingsDecode(r, &v.settings)
	codecSliceOfstringDecode(r, &v.allowed)
	codecIntDecode(r, &v.maxSteps)
	codecTimeTimeDecode(r, &v.createdAt)
	codecTimeTimeDecode(r, &v.updatedAt)
	codecMapOfinputDecode(r, &v.inputs)
	codecProtocolProgressDecode(r, &v.prog)
	codecTurnDecode(r, &v.turn)
	codecMapOfboolDecode(r, &v.turnIDs)
	codecProtocolTurnEndedDecode(r, &v.lastEnded)
	codecSliceOfOpenToolCallDecode(r, &v.calls)
	codecMapOfpendingRequestDecode(r, &v.requests)
	codecMapOfProtocolRequestResolvedDecode(r, &v.resolved)
	codecMapOfJSONRawMessageDecode(r, &v.asked)
	codecStringDecode(r, &v.evaluated)
	codecProtocolUsageDecode(r, &v.usage)
	codecProtocolContextMeasuredDecode(r, &v.context)
	codecIntDecode(r, &v.measuredTurn)
	codecTimeTimeDecode(r, &v.readAt)
	codecTimeDurationDecode(r, &v.readTTL)
	codecPtrToProtocolSubscriptionUsageDecode(r, &v.subscribed)
	codecPtrTofloat64Decode(r, &v.cost)
	codecProtocolCompactionAppliedDecode(r, &v.compaction)
	codecUint64Decode(r, &v.compactedAt)
	codecTimeTimeDecode(r, &v.compactedTime)
	codecIntDecode(r, &v.compacted)
	codecIntDecode(r, &v.compactedLen)
	codecMapOfProtocolOutcomeDecode(r, &v.children)
	codecMapOfBackendChainDecode(r, &v.backends)
	codecSliceOfProtocolToolResultRetainedDecode(r, &v.retained)
	codecMapOfcommandDecode(r, &v.commands)
	codecSliceOfentryDecode(r, &v.history)
	codecSliceOfProtocolMessageDecode(r, &v.folded)
	codecSliceOfentryDecode(r, &v.stranded)
	codecIntDecode(r, &v.turnAt)
	codecStringDecode(r, &v.turnBy)
	codecIntDecode(r, &v.turnN)
	codecIntDecode(r, &v.turnItems)
	codecSliceOfintDecode(r, &v.unran)
}

func codecLightStateEncode(w *codecWriter, v LightState) {
	codecStateEncode(w, v.state)
	codecSliceOflightEntryEncode(w, v.folded)
	codecMapOfattachmentInfoEncode(w, v.attachments)
	codecMapOfuint64Encode(w, v.asked)
	codecMapOfuint64Encode(w, v.resolved)
	codecMapOfuint64Encode(w, v.commands)
	codecSliceOfuint64Encode(w, v.retained)
}
func codecLightStateDecode(r *codecReader, v *LightState) {
	codecStateDecode(r, &v.state)
	codecSliceOflightEntryDecode(r, &v.folded)
	codecMapOfattachmentInfoDecode(r, &v.attachments)
	codecMapOfuint64Decode(r, &v.asked)
	codecMapOfuint64Decode(r, &v.resolved)
	codecMapOfuint64Decode(r, &v.commands)
	codecSliceOfuint64Decode(r, &v.retained)
}

func codecBoolEncode(w *codecWriter, v bool) {
	w.boolean(bool(v))
}
func codecBoolDecode(r *codecReader, v *bool) {
	*v = bool(r.boolean())
}

func codecStringEncode(w *codecWriter, v string) {
	w.string(string(v))
}
func codecStringDecode(r *codecReader, v *string) {
	*v = string(r.string())
}

func codecProtocolSettingsEncode(w *codecWriter, v protocol.Settings) {
	codecStringEncode(w, v.Effort)
	codecStringEncode(w, v.ServiceTier)
	codecMapOfstringEncode(w, v.Accounts)
}
func codecProtocolSettingsDecode(r *codecReader, v *protocol.Settings) {
	codecStringDecode(r, &v.Effort)
	codecStringDecode(r, &v.ServiceTier)
	codecMapOfstringDecode(r, &v.Accounts)
}

func codecSliceOfstringEncode(w *codecWriter, v []string) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecStringEncode(w, x)
	}
}
func codecSliceOfstringDecode(r *codecReader, v *[]string) {
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

func codecIntEncode(w *codecWriter, v int) {
	w.signed(int64(v))
}
func codecIntDecode(r *codecReader, v *int) {
	*v = int(r.signed())
}

func codecTimeTimeEncode(w *codecWriter, v time.Time) {
	b, err := v.MarshalBinary()
	if err != nil {
		w.fail(err)
	}
	w.bytes(b)
}
func codecTimeTimeDecode(r *codecReader, v *time.Time) {
	b := r.bytes()
	if r.err == nil {
		r.fail(v.UnmarshalBinary(b))
	}
}

func codecMapOfinputEncode(w *codecWriter, v map[string]input) {
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
		codecInputEncode(w, v[string(k)])
	}
}
func codecMapOfinputDecode(r *codecReader, v *map[string]input) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(input))) {
		return
	}
	*v = make(map[string]input, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x input
		codecInputDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecProtocolProgressEncode(w *codecWriter, v protocol.Progress) {
	codecUint64Encode(w, v.Seq)
	codecProtocolStatusEncode(w, v.Status)
	codecStringEncode(w, v.TurnID)
	codecSliceOfstringEncode(w, v.Queued)
	codecSliceOfstringEncode(w, v.Requests)
	codecPtrToProtocolGoalViewEncode(w, v.Goal)
	codecPtrToProtocolLastTurnEncode(w, v.LastTurn)
}
func codecProtocolProgressDecode(r *codecReader, v *protocol.Progress) {
	codecUint64Decode(r, &v.Seq)
	codecProtocolStatusDecode(r, &v.Status)
	codecStringDecode(r, &v.TurnID)
	codecSliceOfstringDecode(r, &v.Queued)
	codecSliceOfstringDecode(r, &v.Requests)
	codecPtrToProtocolGoalViewDecode(r, &v.Goal)
	codecPtrToProtocolLastTurnDecode(r, &v.LastTurn)
}

func codecTurnEncode(w *codecWriter, v Turn) {
	codecStringEncode(w, v.ID)
	codecSliceOfstringEncode(w, v.InputIDs)
	codecBoolEncode(w, v.Suspended)
	codecIntEncode(w, v.Resumes)
}
func codecTurnDecode(r *codecReader, v *Turn) {
	codecStringDecode(r, &v.ID)
	codecSliceOfstringDecode(r, &v.InputIDs)
	codecBoolDecode(r, &v.Suspended)
	codecIntDecode(r, &v.Resumes)
}

func codecMapOfboolEncode(w *codecWriter, v map[string]bool) {
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
		codecBoolEncode(w, v[string(k)])
	}
}
func codecMapOfboolDecode(r *codecReader, v *map[string]bool) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(bool))) {
		return
	}
	*v = make(map[string]bool, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x bool
		codecBoolDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecProtocolTurnEndedEncode(w *codecWriter, v protocol.TurnEnded) {
	codecStringEncode(w, v.TurnID)
	codecProtocolStopReasonEncode(w, v.StopReason)
	codecProtocolCauseEncode(w, v.Cause)
	codecBoolEncode(w, v.TaskCancel)
	codecStringEncode(w, v.Error)
	codecProtocolErrorClassEncode(w, v.ErrorClass)
	codecStringEncode(w, v.RecoverHint)
	codecInt64Encode(w, v.ResetsAt)
	codecStringEncode(w, v.ErrorDetail)
}
func codecProtocolTurnEndedDecode(r *codecReader, v *protocol.TurnEnded) {
	codecStringDecode(r, &v.TurnID)
	codecProtocolStopReasonDecode(r, &v.StopReason)
	codecProtocolCauseDecode(r, &v.Cause)
	codecBoolDecode(r, &v.TaskCancel)
	codecStringDecode(r, &v.Error)
	codecProtocolErrorClassDecode(r, &v.ErrorClass)
	codecStringDecode(r, &v.RecoverHint)
	codecInt64Decode(r, &v.ResetsAt)
	codecStringDecode(r, &v.ErrorDetail)
}

func codecSliceOfOpenToolCallEncode(w *codecWriter, v []OpenToolCall) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecOpenToolCallEncode(w, x)
	}
}
func codecSliceOfOpenToolCallDecode(r *codecReader, v *[]OpenToolCall) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(OpenToolCall{})) {
		return
	}
	*v = make([]OpenToolCall, n)
	for i := range *v {
		codecOpenToolCallDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecMapOfpendingRequestEncode(w *codecWriter, v map[string]pendingRequest) {
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
		codecPendingRequestEncode(w, v[string(k)])
	}
}
func codecMapOfpendingRequestDecode(r *codecReader, v *map[string]pendingRequest) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(pendingRequest))) {
		return
	}
	*v = make(map[string]pendingRequest, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x pendingRequest
		codecPendingRequestDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecMapOfProtocolRequestResolvedEncode(w *codecWriter, v map[string]protocol.RequestResolved) {
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
		codecProtocolRequestResolvedEncode(w, v[string(k)])
	}
}
func codecMapOfProtocolRequestResolvedDecode(r *codecReader, v *map[string]protocol.RequestResolved) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(protocol.RequestResolved))) {
		return
	}
	*v = make(map[string]protocol.RequestResolved, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x protocol.RequestResolved
		codecProtocolRequestResolvedDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

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
	if !r.allocateMap(n, unsafe.Sizeof(*new(json.RawMessage))) {
		return
	}
	*v = make(map[string]json.RawMessage, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x json.RawMessage
		codecJSONRawMessageDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecProtocolUsageEncode(w *codecWriter, v protocol.Usage) {
	codecInt64Encode(w, v.InputTokens)
	codecInt64Encode(w, v.OutputTokens)
	codecInt64Encode(w, v.CacheReadTokens)
	codecInt64Encode(w, v.CacheWriteTokens)
	codecInt64Encode(w, v.WebSearchRequests)
}
func codecProtocolUsageDecode(r *codecReader, v *protocol.Usage) {
	codecInt64Decode(r, &v.InputTokens)
	codecInt64Decode(r, &v.OutputTokens)
	codecInt64Decode(r, &v.CacheReadTokens)
	codecInt64Decode(r, &v.CacheWriteTokens)
	codecInt64Decode(r, &v.WebSearchRequests)
}

func codecProtocolContextMeasuredEncode(w *codecWriter, v protocol.ContextMeasured) {
	codecInt64Encode(w, v.Tokens)
	codecInt64Encode(w, v.Window)
	codecBoolEncode(w, v.WindowEstimated)
	codecStringEncode(w, v.Source)
	codecInt64Encode(w, v.CacheTTLSeconds)
	codecProtocolUsageEncode(w, v.Usage)
	codecPtrToProtocolSubscriptionUsageEncode(w, v.SubscriptionUsage)
	codecPtrTofloat64Encode(w, v.CostUSD)
}
func codecProtocolContextMeasuredDecode(r *codecReader, v *protocol.ContextMeasured) {
	codecInt64Decode(r, &v.Tokens)
	codecInt64Decode(r, &v.Window)
	codecBoolDecode(r, &v.WindowEstimated)
	codecStringDecode(r, &v.Source)
	codecInt64Decode(r, &v.CacheTTLSeconds)
	codecProtocolUsageDecode(r, &v.Usage)
	codecPtrToProtocolSubscriptionUsageDecode(r, &v.SubscriptionUsage)
	codecPtrTofloat64Decode(r, &v.CostUSD)
}

func codecTimeDurationEncode(w *codecWriter, v time.Duration) {
	w.signed(int64(v))
}
func codecTimeDurationDecode(r *codecReader, v *time.Duration) {
	*v = time.Duration(r.signed())
}
