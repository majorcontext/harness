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
	if !r.allocate(n, unsafe.Sizeof(protocol.SubscriptionUsageWindow{})) {
		return
	}
	*v = make([]protocol.SubscriptionUsageWindow, n)
	for i := range *v {
		codecProtocolSubscriptionUsageWindowDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
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
		if !r.allocate(1, unsafe.Sizeof(*new(protocol.SubscriptionOverage))) {
			return
		}
		v2 := new(protocol.SubscriptionOverage)
		codecProtocolSubscriptionOverageDecode(r, v2)
		*v = v2
	} else {
		*v = nil
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

func codecProtocolLogMessageEncode(w *codecWriter, v protocol.LogMessage) {
	codecStringEncode(w, v.Role)
	codecSliceOfProtocolLogPartEncode(w, v.Parts)
}
func codecProtocolLogMessageDecode(r *codecReader, v *protocol.LogMessage) {
	codecStringDecode(r, &v.Role)
	codecSliceOfProtocolLogPartDecode(r, &v.Parts)
}

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
	if !r.allocate(n, unsafe.Sizeof(protocol.InputAdmitted{})) {
		return
	}
	*v = make([]protocol.InputAdmitted, n)
	for i := range *v {
		codecProtocolInputAdmittedDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
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
	if !r.allocate(n, unsafe.Sizeof(protocol.MessagePart{})) {
		return
	}
	*v = make([]protocol.MessagePart, n)
	for i := range *v {
		codecProtocolMessagePartDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
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
	if !r.allocate(n, unsafe.Sizeof(protocol.OperatorBatchEntry{})) {
		return
	}
	*v = make([]protocol.OperatorBatchEntry, n)
	for i := range *v {
		codecProtocolOperatorBatchEntryDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
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
	if !r.allocate(n, unsafe.Sizeof(protocol.LogPart{})) {
		return
	}
	*v = make([]protocol.LogPart, n)
	for i := range *v {
		codecProtocolLogPartDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
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
