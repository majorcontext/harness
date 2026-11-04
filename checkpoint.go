package harness

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

type checkpointStore interface {
	LoadCheckpoint(context.Context, string) (uint64, []byte, error)
	SaveCheckpoint(context.Context, string, uint64, []byte) error
}

type sequenceReader interface {
	ReadSeqs(context.Context, string, []uint64) ([]Record, error)
}

const checkpointTail = 512

func openCheckpointView(ctx context.Context, st Store, checkpoints checkpointStore, id string, windows func(string) turn.Capabilities, threshold float64) (*View, error) {
	head, err := st.Head(ctx, id)
	if err != nil {
		return nil, err
	}
	if head == 0 {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	s := eventlog.NewLightState()
	if eventlog.CheckpointVersion() != "" {
		seq, data, err := checkpoints.LoadCheckpoint(ctx, id)
		if err == nil && seq > 0 && seq <= head {
			if restored, err := eventlog.RestoreLightState(data); err == nil && restored.Head() == seq {
				s = restored
			}
		}
	} else {
		checkpoints = nil
	}
	start := s.Head()
	if err := session.ReplayLight(ctx, storeLog{st, id}, s, head); err != nil {
		if start == 0 || !errors.Is(err, ErrUnreplayable) {
			return nil, err
		}
		s = eventlog.NewLightState()
		start = 0
		if err := session.ReplayLight(ctx, storeLog{st, id}, s, head); err != nil {
			return nil, err
		}
	}
	if checkpoints != nil && (start == 0 || head-start >= checkpointTail) {
		if data, err := s.Checkpoint(); err == nil {
			_ = checkpoints.SaveCheckpoint(ctx, id, head, data)
		}
	}
	var caps turn.Capabilities
	if windows != nil {
		caps = windows(s.Model())
	}
	return &View{st: st, id: id, state: session.Describe(id, s, caps, threshold), light: s, restored: start > 0}, nil
}

func (v *View) lightMessages(ctx context.Context, before uint64, limit int) (protocol.MessagePage, error) {
	plan, seqs := v.light.PageSources(before, limit)
	records, err := readPageSources(ctx, v.st, v.id, seqs)
	if err != nil {
		return protocol.MessagePage{}, err
	}
	page, err := v.light.RenderPage(plan, records)
	if err == nil || !v.restored {
		return page, err
	}
	s, err := session.LoadThrough(ctx, storeLog{v.st, v.id}, v.state.HeadSeq)
	if err != nil {
		return protocol.MessagePage{}, err
	}
	return s.MessagePage(before, limit), nil
}

func readPageSources(ctx context.Context, st Store, id string, seqs []uint64) ([]eventlog.Record, error) {
	seqs = slices.Clone(seqs)
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	if len(seqs) == 0 {
		return nil, nil
	}
	if seqs[0] == 0 {
		return nil, fmt.Errorf("%w: invalid page source sequence", ErrUnreplayable)
	}
	var records []Record
	if reader, ok := st.(sequenceReader); ok {
		var err error
		records, err = reader.ReadSeqs(ctx, id, seqs)
		if err != nil {
			return nil, err
		}
	} else {
		for i := 0; i < len(seqs); {
			j := i + 1
			for j < len(seqs) && seqs[j] == seqs[j-1]+1 {
				j++
			}
			for after := seqs[i] - 1; after < seqs[j-1]; {
				r, err := st.Read(ctx, id, after, int(seqs[j-1]-after))
				if err != nil {
					return nil, err
				}
				if len(r) == 0 || r[0].Seq != after+1 || r[len(r)-1].Seq <= after {
					return nil, fmt.Errorf("%w: missing page source record %d", ErrUnreplayable, after+1)
				}
				records = append(records, r...)
				after = r[len(r)-1].Seq
			}
			i = j
		}
	}
	slices.SortFunc(records, func(a, b Record) int { return cmp.Compare(a.Seq, b.Seq) })
	if len(records) != len(seqs) {
		return nil, fmt.Errorf("%w: missing page source records", ErrUnreplayable)
	}
	out := make([]eventlog.Record, len(records))
	for i, r := range records {
		if r.Seq != seqs[i] {
			return nil, fmt.Errorf("%w: missing page source record %d", ErrUnreplayable, seqs[i])
		}
		out[i] = eventlog.Record{Seq: r.Seq, Data: r.Data}
	}
	return out, nil
}
