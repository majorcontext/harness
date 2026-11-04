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

func codecPtrToProtocolSubscriptionUsageEncode(w *codecWriter, v *protocol.SubscriptionUsage) {
	w.boolean(v != nil)
	if v != nil {
		codecProtocolSubscriptionUsageEncode(w, *v)
	}
}
func codecPtrToProtocolSubscriptionUsageDecode(r *codecReader, v **protocol.SubscriptionUsage) {
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

func codecPtrTofloat6421Encode(w *codecWriter, v *float64) {
	w.boolean(v != nil)
	if v != nil {
		codecFloat64Encode(w, *v)
	}
}
func codecPtrTofloat6421Decode(r *codecReader, v **float64) {
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

func codecProtocolCompactionAppliedEncode(w *codecWriter, v protocol.CompactionApplied) {
	codecUint64Encode(w, v.FromSeq)
	codecUint64Encode(w, v.ToSeq)
	codecStringEncode(w, v.Summary)
	codecStringEncode(w, v.Index)
	codecBoolEncode(w, v.ByBackend)
	codecProtocolUsageEncode(w, v.Usage)
}
func codecProtocolCompactionAppliedDecode(r *codecReader, v *protocol.CompactionApplied) {
	codecUint64Decode(r, &v.FromSeq)
	codecUint64Decode(r, &v.ToSeq)
	codecStringDecode(r, &v.Summary)
	codecStringDecode(r, &v.Index)
	codecBoolDecode(r, &v.ByBackend)
	codecProtocolUsageDecode(r, &v.Usage)
}

func codecUint64Encode(w *codecWriter, v uint64) {
	w.unsigned(uint64(v))
}
func codecUint64Decode(r *codecReader, v *uint64) {
	*v = uint64(r.unsigned())
}

func codecMapOfProtocolOutcomeEncode(w *codecWriter, v map[string]protocol.Outcome) {
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
		codecProtocolOutcomeEncode(w, v[string(k)])
	}
}
func codecMapOfProtocolOutcomeDecode(r *codecReader, v *map[string]protocol.Outcome) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(protocol.Outcome))) {
		return
	}
	*v = make(map[string]protocol.Outcome, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x protocol.Outcome
		codecProtocolOutcomeDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecMapOfBackendChainEncode(w *codecWriter, v map[string]BackendChain) {
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
		codecBackendChainEncode(w, v[string(k)])
	}
}
func codecMapOfBackendChainDecode(r *codecReader, v *map[string]BackendChain) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(BackendChain))) {
		return
	}
	*v = make(map[string]BackendChain, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x BackendChain
		codecBackendChainDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecSliceOfProtocolToolResultRetainedEncode(w *codecWriter, v []protocol.ToolResultRetained) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolToolResultRetainedEncode(w, x)
	}
}
func codecSliceOfProtocolToolResultRetainedDecode(r *codecReader, v *[]protocol.ToolResultRetained) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(protocol.ToolResultRetained{})) {
		return
	}
	*v = make([]protocol.ToolResultRetained, n)
	for i := range *v {
		codecProtocolToolResultRetainedDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecMapOfcommandEncode(w *codecWriter, v map[string]command) {
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
		codecCommandEncode(w, v[string(k)])
	}
}
func codecMapOfcommandDecode(r *codecReader, v *map[string]command) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(command))) {
		return
	}
	*v = make(map[string]command, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x command
		codecCommandDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecSliceOfentryEncode(w *codecWriter, v []entry) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecEntryEncode(w, x)
	}
}
func codecSliceOfentryDecode(r *codecReader, v *[]entry) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(entry{})) {
		return
	}
	*v = make([]entry, n)
	for i := range *v {
		codecEntryDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecSliceOfProtocolMessageEncode(w *codecWriter, v []protocol.Message) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecProtocolMessageEncode(w, x)
	}
}
func codecSliceOfProtocolMessageDecode(r *codecReader, v *[]protocol.Message) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(protocol.Message{})) {
		return
	}
	*v = make([]protocol.Message, n)
	for i := range *v {
		codecProtocolMessageDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecSliceOfentry30Encode(w *codecWriter, v []entry) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecEntryEncode(w, x)
	}
}
func codecSliceOfentry30Decode(r *codecReader, v *[]entry) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(entry{})) {
		return
	}
	*v = make([]entry, n)
	for i := range *v {
		codecEntryDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecSliceOfintEncode(w *codecWriter, v []int) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecIntEncode(w, x)
	}
}
func codecSliceOfintDecode(r *codecReader, v *[]int) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(*new(int))) {
		return
	}
	*v = make([]int, n)
	for i := range *v {
		codecIntDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecSliceOflightEntryEncode(w *codecWriter, v []lightEntry) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecLightEntryEncode(w, x)
	}
}
func codecSliceOflightEntryDecode(r *codecReader, v *[]lightEntry) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(lightEntry{})) {
		return
	}
	*v = make([]lightEntry, n)
	for i := range *v {
		codecLightEntryDecode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecMapOfattachmentInfoEncode(w *codecWriter, v map[string]attachmentInfo) {
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
		codecAttachmentInfoEncode(w, v[string(k)])
	}
}
func codecMapOfattachmentInfoDecode(r *codecReader, v *map[string]attachmentInfo) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(attachmentInfo))) {
		return
	}
	*v = make(map[string]attachmentInfo, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x attachmentInfo
		codecAttachmentInfoDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecMapOfuint64Encode(w *codecWriter, v map[string]uint64) {
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
		codecUint64Encode(w, v[string(k)])
	}
}
func codecMapOfuint64Decode(r *codecReader, v *map[string]uint64) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(uint64))) {
		return
	}
	*v = make(map[string]uint64, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x uint64
		codecUint64Decode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecMapOfuint6435Encode(w *codecWriter, v map[string]uint64) {
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
		codecUint64Encode(w, v[string(k)])
	}
}
func codecMapOfuint6435Decode(r *codecReader, v *map[string]uint64) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(uint64))) {
		return
	}
	*v = make(map[string]uint64, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x uint64
		codecUint64Decode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecMapOfuint6436Encode(w *codecWriter, v map[string]uint64) {
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
		codecUint64Encode(w, v[string(k)])
	}
}
func codecMapOfuint6436Decode(r *codecReader, v *map[string]uint64) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(uint64))) {
		return
	}
	*v = make(map[string]uint64, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x uint64
		codecUint64Decode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecSliceOfuint6437Encode(w *codecWriter, v []uint64) {
	if v == nil {
		w.length(-1)
		return
	}
	w.length(len(v))
	for _, x := range v {
		codecUint64Encode(w, x)
	}
}
func codecSliceOfuint6437Decode(r *codecReader, v *[]uint64) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocate(n, unsafe.Sizeof(*new(uint64))) {
		return
	}
	*v = make([]uint64, n)
	for i := range *v {
		codecUint64Decode(r, &(*v)[i])
		if r.err != nil {
			return
		}
	}
}

func codecMapOfstringEncode(w *codecWriter, v map[string]string) {
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
		codecStringEncode(w, v[string(k)])
	}
}
func codecMapOfstringDecode(r *codecReader, v *map[string]string) {
	n := r.length()
	if n < 0 {
		*v = nil
		return
	}
	if !r.count(n) {
		return
	}
	if !r.allocateMap(n, unsafe.Sizeof(*new(string))) {
		return
	}
	*v = make(map[string]string, n)
	for i := 0; i < n; i++ {
		k := string(r.string())
		var x string
		codecStringDecode(r, &x)
		if r.err != nil {
			return
		}
		(*v)[k] = x
	}
}

func codecInputEncode(w *codecWriter, v input) {
	codecInputStateEncode(w, v.state)
	codecUint64Encode(w, v.seq)
	codecProtocolInputAdmittedEncode(w, v.event)
	codecStringEncode(w, v.turnID)
}
func codecInputDecode(r *codecReader, v *input) {
	codecInputStateDecode(r, &v.state)
	codecUint64Decode(r, &v.seq)
	codecProtocolInputAdmittedDecode(r, &v.event)
	codecStringDecode(r, &v.turnID)
}
