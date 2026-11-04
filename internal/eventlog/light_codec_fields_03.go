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

func codecProtocolGoalViewEncode(w *codecWriter, v protocol.GoalView) {
	codecProtocolGoalEncode(w, v.Goal)
	codecProtocolGoalStateEncode(w, v.State)
	codecIntEncode(w, v.Turns)
	codecIntEncode(w, v.Pauses)
	codecProtocolVerdictEncode(w, v.Verdict)
	codecStringEncode(w, v.Reason)
	codecTimeTimeEncode(w, v.RetryAt)
}
func codecProtocolGoalViewDecode(r *codecReader, v *protocol.GoalView) {
	codecProtocolGoalDecode(r, &v.Goal)
	codecProtocolGoalStateDecode(r, &v.State)
	codecIntDecode(r, &v.Turns)
	codecIntDecode(r, &v.Pauses)
	codecProtocolVerdictDecode(r, &v.Verdict)
	codecStringDecode(r, &v.Reason)
	codecTimeTimeDecode(r, &v.RetryAt)
}

func codecProtocolLastTurnEncode(w *codecWriter, v protocol.LastTurn) {
	codecStringEncode(w, v.TurnID)
	codecProtocolStopReasonEncode(w, v.StopReason)
	codecProtocolCauseEncode(w, v.Cause)
	codecStringEncode(w, v.Error)
}
func codecProtocolLastTurnDecode(r *codecReader, v *protocol.LastTurn) {
	codecStringDecode(r, &v.TurnID)
	codecProtocolStopReasonDecode(r, &v.StopReason)
	codecProtocolCauseDecode(r, &v.Cause)
	codecStringDecode(r, &v.Error)
}

func codecProtocolRequestOpenedEncode(w *codecWriter, v protocol.RequestOpened) {
	codecStringEncode(w, v.RequestID)
	codecStringEncode(w, v.ItemID)
	codecStringEncode(w, v.RequestKind)
	codecJSONRawMessageEncode(w, v.Payload)
}
func codecProtocolRequestOpenedDecode(r *codecReader, v *protocol.RequestOpened) {
	codecStringDecode(r, &v.RequestID)
	codecStringDecode(r, &v.ItemID)
	codecStringDecode(r, &v.RequestKind)
	codecJSONRawMessageDecode(r, &v.Payload)
}

func codecProtocolResolutionKindEncode(w *codecWriter, v protocol.ResolutionKind) {
	w.string(string(v))
}
func codecProtocolResolutionKindDecode(r *codecReader, v *protocol.ResolutionKind) {
	*v = protocol.ResolutionKind(r.string())
}

func codecByteEncode(w *codecWriter, v byte) {
	w.unsigned(uint64(v))
}
func codecByteDecode(r *codecReader, v *byte) {
	*v = byte(r.unsigned())
}

func codecSliceOfProtocolSubscriptionUsageWindowEncode(w *codecWriter, v []protocol.SubscriptionUsageWindow) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolSubscriptionUsageWindowEncode(w, x)
	}
}
func codecSliceOfProtocolSubscriptionUsageWindowDecode(r *codecReader, v *[]protocol.SubscriptionUsageWindow) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]protocol.SubscriptionUsageWindow, n)
	for i := range *v {
		codecProtocolSubscriptionUsageWindowDecode(r, &(*v)[i])
	}
}

func codecPtrToProtocolSubscriptionOverageEncode(w *codecWriter, v *protocol.SubscriptionOverage) {
	w.boolean(v != nil)
	if v != nil {
		codecProtocolSubscriptionOverageEncode(w, *v)
	}
}
func codecPtrToProtocolSubscriptionOverageDecode(r *codecReader, v **protocol.SubscriptionOverage) {
	if r.boolean() {
		v2 := new(protocol.SubscriptionOverage)
		codecProtocolSubscriptionOverageDecode(r, v2)
		*v = v2
	} else {
		*v = nil
	}
}

func codecPtrTofloat64Encode(w *codecWriter, v *float64) {
	w.boolean(v != nil)
	if v != nil {
		codecFloat64Encode(w, *v)
	}
}
func codecPtrTofloat64Decode(r *codecReader, v **float64) {
	if r.boolean() {
		v2 := new(float64)
		codecFloat64Decode(r, v2)
		*v = v2
	} else {
		*v = nil
	}
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
	*v = make([]string, n)
	for i := range *v {
		codecStringDecode(r, &(*v)[i])
	}
}

func codecProtocolCommandRecordedEncode(w *codecWriter, v protocol.CommandRecorded) {
	codecStringEncode(w, v.InputID)
	codecStringEncode(w, v.Line)
	codecStringEncode(w, v.Name)
	codecMapOfanyEncode(w, v.Args)
	codecStringEncode(w, v.Status)
	codecStringEncode(w, v.Text)
	codecJSONRawMessageEncode(w, v.Result)
	codecBoolEncode(w, v.ResultTruncated)
}
func codecProtocolCommandRecordedDecode(r *codecReader, v *protocol.CommandRecorded) {
	codecStringDecode(r, &v.InputID)
	codecStringDecode(r, &v.Line)
	codecStringDecode(r, &v.Name)
	codecMapOfanyDecode(r, &v.Args)
	codecStringDecode(r, &v.Status)
	codecStringDecode(r, &v.Text)
	codecJSONRawMessageDecode(r, &v.Result)
	codecBoolDecode(r, &v.ResultTruncated)
}

func codecSliceOfuint64Encode(w *codecWriter, v []uint64) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecUint64Encode(w, x)
	}
}
func codecSliceOfuint64Decode(r *codecReader, v *[]uint64) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	*v = make([]uint64, n)
	for i := range *v {
		codecUint64Decode(r, &(*v)[i])
	}
}

func codecProtocolLogMessageEncode(w *codecWriter, v protocol.LogMessage) {
	codecStringEncode(w, v.Role)
	codecSliceOfProtocolLogPart92Encode(w, v.Parts)
}
func codecProtocolLogMessageDecode(r *codecReader, v *protocol.LogMessage) {
	codecStringDecode(r, &v.Role)
	codecSliceOfProtocolLogPart92Decode(r, &v.Parts)
}
