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

func codecProtocolToolResultRetainedEncode(w *codecWriter, v protocol.ToolResultRetained) {
	codecStringEncode(w, v.Handle)
	codecStringEncode(w, v.Tool)
	codecStringEncode(w, v.BlobKey)
	codecIntEncode(w, v.Bytes)
	codecIntEncode(w, v.Lines)
	codecStringEncode(w, v.Head)
}
func codecProtocolToolResultRetainedDecode(r *codecReader, v *protocol.ToolResultRetained) {
	codecStringDecode(r, &v.Handle)
	codecStringDecode(r, &v.Tool)
	codecStringDecode(r, &v.BlobKey)
	codecIntDecode(r, &v.Bytes)
	codecIntDecode(r, &v.Lines)
	codecStringDecode(r, &v.Head)
}

func codecCommandEncode(w *codecWriter, v command) {
	codecProtocolCommandRecordedEncode(w, v.rec)
	codecUint64Encode(w, v.seq)
	codecStringEncode(w, v.after)
}
func codecCommandDecode(r *codecReader, v *command) {
	codecProtocolCommandRecordedDecode(r, &v.rec)
	codecUint64Decode(r, &v.seq)
	codecStringDecode(r, &v.after)
}

func codecEntryEncode(w *codecWriter, v entry) {
	codecUint64Encode(w, v.seq)
	codecSliceOfuint64Encode(w, v.sources)
	codecTimeTimeEncode(w, v.at)
	codecStringEncode(w, v.id)
	codecProtocolLogMessageEncode(w, v.msg)
	codecStringEncode(w, v.by)
	codecIntEncode(w, v.turn)
	codecSliceOfProtocolInputAdmittedEncode(w, v.promoted)
	codecUint64Encode(w, v.last)
	codecProvenanceEncode(w, v.from)
	codecBoolEncode(w, v.pinned)
	codecBoolEncode(w, v.readerOnly)
	codecBoolEncode(w, v.carried)
}
func codecEntryDecode(r *codecReader, v *entry) {
	codecUint64Decode(r, &v.seq)
	codecSliceOfuint64Decode(r, &v.sources)
	codecTimeTimeDecode(r, &v.at)
	codecStringDecode(r, &v.id)
	codecProtocolLogMessageDecode(r, &v.msg)
	codecStringDecode(r, &v.by)
	codecIntDecode(r, &v.turn)
	codecSliceOfProtocolInputAdmittedDecode(r, &v.promoted)
	codecUint64Decode(r, &v.last)
	codecProvenanceDecode(r, &v.from)
	codecBoolDecode(r, &v.pinned)
	codecBoolDecode(r, &v.readerOnly)
	codecBoolDecode(r, &v.carried)
}

func codecProtocolMessageEncode(w *codecWriter, v protocol.Message) {
	codecStringEncode(w, v.ID)
	codecStringEncode(w, v.Role)
	codecSliceOfProtocolMessagePartEncode(w, v.Parts)
	codecTimeTimeEncode(w, v.CreatedAt)
	codecStringEncode(w, v.Source)
	codecStringEncode(w, v.SourceID)
	codecStringEncode(w, v.SourceLabel)
	codecSliceOfProtocolOperatorBatchEntryEncode(w, v.OperatorBatch)
}
func codecProtocolMessageDecode(r *codecReader, v *protocol.Message) {
	codecStringDecode(r, &v.ID)
	codecStringDecode(r, &v.Role)
	codecSliceOfProtocolMessagePartDecode(r, &v.Parts)
	codecTimeTimeDecode(r, &v.CreatedAt)
	codecStringDecode(r, &v.Source)
	codecStringDecode(r, &v.SourceID)
	codecStringDecode(r, &v.SourceLabel)
	codecSliceOfProtocolOperatorBatchEntryDecode(r, &v.OperatorBatch)
}

func codecLightEntryEncode(w *codecWriter, v lightEntry) {
	codecEntryEncode(w, v.Entry)
	codecUint64Encode(w, v.Summary)
	codecTimeTimeEncode(w, v.At)
}
func codecLightEntryDecode(r *codecReader, v *lightEntry) {
	codecEntryDecode(r, &v.Entry)
	codecUint64Decode(r, &v.Summary)
	codecTimeTimeDecode(r, &v.At)
}

func codecAttachmentInfoEncode(w *codecWriter, v attachmentInfo) {
	codecStringEncode(w, v.MediaType)
	codecIntEncode(w, v.Size)
}
func codecAttachmentInfoDecode(r *codecReader, v *attachmentInfo) {
	codecStringDecode(r, &v.MediaType)
	codecIntDecode(r, &v.Size)
}

func codecInputStateEncode(w *codecWriter, v inputState) {
	w.string(string(v))
}
func codecInputStateDecode(r *codecReader, v *inputState) {
	*v = inputState(r.string())
}

func codecProtocolInputAdmittedEncode(w *codecWriter, v protocol.InputAdmitted) {
	codecStringEncode(w, v.InputID)
	codecProtocolDeliveryEncode(w, v.Delivery)
	codecStringEncode(w, v.Source)
	codecStringEncode(w, v.SourceID)
	codecStringEncode(w, v.SourceLabel)
	codecSliceOfProtocolLogPartEncode(w, v.Parts)
	codecStringEncode(w, v.Traceparent)
	codecStringEncode(w, v.Tracestate)
}
func codecProtocolInputAdmittedDecode(r *codecReader, v *protocol.InputAdmitted) {
	codecStringDecode(r, &v.InputID)
	codecProtocolDeliveryDecode(r, &v.Delivery)
	codecStringDecode(r, &v.Source)
	codecStringDecode(r, &v.SourceID)
	codecStringDecode(r, &v.SourceLabel)
	codecSliceOfProtocolLogPartDecode(r, &v.Parts)
	codecStringDecode(r, &v.Traceparent)
	codecStringDecode(r, &v.Tracestate)
}
