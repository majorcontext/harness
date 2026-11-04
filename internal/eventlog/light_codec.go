package eventlog

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/gob"
	"encoding/json"
	"errors"
	"io"
	"runtime/debug"
)

const maxCheckpointBytes = 128 << 20

type checkpointWriter struct {
	writer io.Writer
	count  int
}

func (w *checkpointWriter) Write(p []byte) (int, error) {
	if len(p) > maxCheckpointBytes-w.count {
		return 0, errors.New("checkpoint too large")
	}
	n, err := w.writer.Write(p)
	w.count += n
	return n, err
}

type checkpointEnvelope struct {
	Version  string            `json:"version"`
	Data     []byte            `json:"data"`
	Checksum [sha256.Size]byte `json:"checksum"`
}

// CheckpointVersion returns the clean VCS revision, or an empty string when unavailable.
func CheckpointVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision, dirty string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value
		}
	}
	if dirty != "false" {
		return ""
	}
	return revision
}

// Checkpoint serializes the light state as a versioned cache.
func (l *LightState) Checkpoint() ([]byte, error) {
	if l == nil {
		return nil, errors.New("nil light state")
	}
	version := CheckpointVersion()
	if version == "" {
		return nil, errors.New("checkpoint version unavailable")
	}
	var packed bytes.Buffer
	z := zlib.NewWriter(&packed)
	if err := gob.NewEncoder(&checkpointWriter{writer: z}).Encode(l.snapshot()); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	if packed.Len() > maxCheckpointBytes {
		return nil, errors.New("checkpoint too large")
	}
	return json.Marshal(checkpointEnvelope{Version: version, Data: packed.Bytes(), Checksum: sha256.Sum256(packed.Bytes())})
}

// RestoreLightState decodes a checkpoint into a light state.
func RestoreLightState(data []byte) (*LightState, error) {
	if len(data) > (maxCheckpointBytes*4/3)+2048 {
		return nil, errors.New("checkpoint too large")
	}
	var encoded checkpointEnvelope
	if err := json.Unmarshal(data, &encoded); err != nil {
		return nil, err
	}
	version := CheckpointVersion()
	if version == "" || encoded.Version != version {
		return nil, errors.New("invalid checkpoint version")
	}
	sum := sha256.Sum256(encoded.Data)
	if !bytes.Equal(encoded.Checksum[:], sum[:]) {
		return nil, errors.New("invalid checkpoint checksum")
	}
	z, err := zlib.NewReader(bytes.NewReader(encoded.Data))
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(z, maxCheckpointBytes+1))
	closeErr := z.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(body) > maxCheckpointBytes {
		return nil, errors.New("checkpoint too large")
	}
	dec := gob.NewDecoder(bytes.NewReader(body))
	var snapshot checkpointSnapshot
	if err := dec.Decode(&snapshot); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing checkpoint data")
	}
	l := snapshot.lightState()
	if err := l.validateCheckpoint(); err != nil {
		return nil, err
	}
	return l, nil
}
