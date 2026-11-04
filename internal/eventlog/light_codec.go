package eventlog

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
)

const checkpointVersion = 2
const maxCheckpointBytes = 128 << 20

type codecWriter struct {
	buf []byte
	err error
}

func (w *codecWriter) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}
func (w *codecWriter) unsigned(v uint64) { w.buf = binary.AppendUvarint(w.buf, v) }
func (w *codecWriter) signed(v int64)    { w.buf = binary.AppendVarint(w.buf, v) }
func (w *codecWriter) length(v int)      { w.signed(int64(v)) }
func (w *codecWriter) boolean(v bool) {
	if v {
		w.unsigned(1)
	} else {
		w.unsigned(0)
	}
}
func (w *codecWriter) float64(v float64) {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, math.Float64bits(v))
}
func (w *codecWriter) bytes(v []byte)  { w.length(len(v)); w.buf = append(w.buf, v...) }
func (w *codecWriter) string(v string) { w.length(len(v)); w.buf = append(w.buf, v...) }

type codecReader struct {
	buf []byte
	err error
}

func (r *codecReader) fail(err error) {
	if err != nil && r.err == nil {
		r.err = err
	}
}
func (r *codecReader) unsigned() uint64 {
	v, n := binary.Uvarint(r.buf)
	if n <= 0 {
		r.fail(errors.New("invalid unsigned integer"))
		return 0
	}
	r.buf = r.buf[n:]
	return v
}
func (r *codecReader) signed() int64 {
	v, n := binary.Varint(r.buf)
	if n <= 0 {
		r.fail(errors.New("invalid signed integer"))
		return 0
	}
	r.buf = r.buf[n:]
	return v
}
func (r *codecReader) length() int {
	v := r.signed()
	if v < -1 || v > maxCheckpointBytes {
		r.fail(errors.New("invalid length"))
		return -1
	}
	return int(v)
}
func (r *codecReader) count(n int) bool {
	if r.err != nil || n > len(r.buf) {
		r.fail(errors.New("invalid count"))
		return false
	}
	return true
}
func (r *codecReader) boolean() bool {
	v := r.unsigned()
	if v > 1 {
		r.fail(errors.New("invalid boolean"))
	}
	return v == 1
}
func (r *codecReader) float64() float64 {
	if len(r.buf) < 8 {
		r.fail(io.ErrUnexpectedEOF)
		return 0
	}
	v := binary.LittleEndian.Uint64(r.buf)
	r.buf = r.buf[8:]
	return math.Float64frombits(v)
}
func (r *codecReader) bytes() []byte {
	n := r.length()
	if n < 0 {
		r.fail(errors.New("invalid bytes"))
		return nil
	}
	if !r.count(n) {
		return nil
	}
	v := r.buf[:n]
	r.buf = r.buf[n:]
	return v
}
func (r *codecReader) string() string { return string(r.bytes()) }

func (w *codecWriter) value(v any) {
	switch x := v.(type) {
	case nil:
		w.unsigned(0)
	case bool:
		w.unsigned(1)
		w.boolean(x)
	case string:
		w.unsigned(2)
		w.string(x)
	case float64:
		w.unsigned(3)
		w.float64(x)
	case json.Number:
		w.unsigned(4)
		w.string(string(x))
	case []any:
		w.unsigned(5)
		w.length(len(x))
		for _, v := range x {
			w.value(v)
		}
	case map[string]any:
		w.unsigned(6)
		w.length(len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w.string(k)
			w.value(x[k])
		}
	default:
		w.fail(fmt.Errorf("unsupported JSON value %T", v))
	}
}
func (r *codecReader) value() any {
	switch r.unsigned() {
	case 0:
		return nil
	case 1:
		return r.boolean()
	case 2:
		return r.string()
	case 3:
		return r.float64()
	case 4:
		return json.Number(r.string())
	case 5:
		n := r.length()
		if n < 0 || !r.count(n) {
			r.fail(errors.New("invalid array"))
			return nil
		}
		v := make([]any, n)
		for i := range v {
			v[i] = r.value()
		}
		return v
	case 6:
		n := r.length()
		if n < 0 || !r.count(n) {
			r.fail(errors.New("invalid object"))
			return nil
		}
		v := make(map[string]any, n)
		for i := 0; i < n; i++ {
			k := r.string()
			v[k] = r.value()
		}
		return v
	default:
		r.fail(errors.New("invalid value tag"))
		return nil
	}
}

func encodeState(s *State) ([]byte, error) {
	if s == nil {
		return nil, errors.New("nil state")
	}
	w := codecWriter{}
	codecStateEncode(&w, *s)
	return w.buf, w.err
}
func decodeState(b []byte) (*State, error) {
	r := codecReader{buf: b}
	s := new(State)
	codecStateDecode(&r, s)
	if r.err != nil {
		return nil, r.err
	}
	if len(r.buf) != 0 {
		return nil, errors.New("trailing state data")
	}
	return s, nil
}

// Checkpoint serializes the light state in a versioned, compressed form.
func (l *LightState) Checkpoint() ([]byte, error) {
	if l == nil {
		return nil, errors.New("nil light state")
	}
	w := codecWriter{}
	codecLightStateEncode(&w, *l)
	if w.err != nil {
		return nil, w.err
	}
	sum := sha256.Sum256(w.buf)
	w.buf = append(w.buf, sum[:]...)
	var out bytes.Buffer
	out.Write([]byte{'H', 'L', 'S', checkpointVersion})
	z := zlib.NewWriter(&out)
	if _, err := z.Write(w.buf); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return []byte(`{"version":` + strconv.Itoa(checkpointVersion) + `,"data":"` + base64.StdEncoding.EncodeToString(out.Bytes()) + `"}`), nil
}

// RestoreLightState decodes a checkpoint into a light state.
func RestoreLightState(data []byte) (*LightState, error) {
	prefix := `{"version":` + strconv.Itoa(checkpointVersion) + `,"data":"`
	if !bytes.HasPrefix(data, []byte(prefix)) || !bytes.HasSuffix(data, []byte(`"}`)) {
		return nil, errors.New("invalid checkpoint version")
	}
	data, err := base64.StdEncoding.DecodeString(string(data[len(prefix) : len(data)-2]))
	if err != nil {
		return nil, err
	}
	if len(data) < 4 || string(data[:3]) != "HLS" || data[3] != checkpointVersion {
		return nil, errors.New("invalid checkpoint version")
	}
	z, err := zlib.NewReader(bytes.NewReader(data[4:]))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(z, maxCheckpointBytes+1))
	closeErr := z.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(b) > maxCheckpointBytes {
		return nil, errors.New("checkpoint too large")
	}
	if len(b) < sha256.Size {
		return nil, errors.New("invalid checkpoint checksum")
	}
	body, checksum := b[:len(b)-sha256.Size], b[len(b)-sha256.Size:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], checksum) {
		return nil, errors.New("invalid checkpoint checksum")
	}
	r := codecReader{buf: body}
	l := new(LightState)
	codecLightStateDecode(&r, l)
	if r.err != nil {
		return nil, r.err
	}
	if len(r.buf) != 0 {
		return nil, errors.New("trailing checkpoint data")
	}
	if err := l.validateCheckpoint(); err != nil {
		return nil, err
	}
	l.state.light = l
	return l, nil
}
