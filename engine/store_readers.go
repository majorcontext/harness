package engine

import (
	"bytes"
	"fmt"
	"time"
)

func diskStoreOf(store SessionStore) (string, bool) {
	d, ok := store.(*DiskStore)
	if !ok {
		return "", false
	}
	return d.Dir(), true
}

func joinedJournal(store SessionStore, id string) ([]byte, error) {
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	recs, err := store.Load(id)
	if err != nil {
		return nil, err
	}
	return append(bytes.Join(recs, []byte("\n")), '\n'), nil
}

func ListSessionIDsFrom(store SessionStore) ([]string, error) {
	ids, err := store.List()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range ids {
		if ValidSessionID(id) {
			out = append(out, id)
		}
	}
	return out, nil
}

func SessionExistsIn(store SessionStore, id string) bool {
	if !ValidSessionID(id) {
		return false
	}
	n, err := store.Len(id)
	return err == nil && n > 0
}

func ReadSessionIndexFrom(store SessionStore, id string) (SessionIndex, error) {
	if dir, ok := diskStoreOf(store); ok {
		return ReadSessionIndex(dir, id)
	}
	data, err := joinedJournal(store, id)
	if err != nil {
		return SessionIndex{}, err
	}
	ix, err := foldSessionJournal(data, time.Time{})
	if err != nil {
		return SessionIndex{}, fmt.Errorf("engine: session %s: %w", id, err)
	}
	ix.ID = id
	return ix, nil
}

func ReadSessionInfoFrom(store SessionStore, id string) (SessionInfo, error) {
	if dir, ok := diskStoreOf(store); ok {
		return ReadSessionInfo(dir, id)
	}
	ix, err := ReadSessionIndexFrom(store, id)
	if err != nil {
		return SessionInfo{}, err
	}
	return SessionInfo{
		ID:               ix.ID,
		CreatedAt:        ix.CreatedAt,
		Messages:         ix.Messages,
		Usage:            ix.Usage,
		LastInputTokens:  ix.LastInputTokens,
		LastPromptTokens: ix.LastPromptTokens,
		WindowTokens:     ix.WindowTokens,
		ContextUnknown:   ix.ContextUnknown,
	}, nil
}

func ReadMessagePageFrom(store SessionStore, id string, beforeSeq, limit int) (MessagePage, error) {
	if dir, ok := diskStoreOf(store); ok {
		return ReadMessagePage(dir, id, beforeSeq, limit)
	}
	data, err := joinedJournal(store, id)
	if err != nil {
		return MessagePage{}, err
	}
	ix, err := foldSessionJournal(data, time.Time{})
	if err != nil {
		return MessagePage{}, fmt.Errorf("engine: session %s: %w", id, err)
	}
	r := bytes.NewReader(data)
	return readMessagePageFrom(func() (pageSource, error) {
		return pageSource{
			ReaderAt: r,
			size:     func() (int64, error) { return r.Size(), nil },
			close:    func() error { return nil },
		}, nil
	}, id, ix, beforeSeq, limit)
}

func LoadJournalFrom(store SessionStore, id string) ([]JournalRecord, error) {
	if dir, ok := diskStoreOf(store); ok {
		return LoadJournal(dir, id)
	}
	data, err := joinedJournal(store, id)
	if err != nil {
		return nil, err
	}
	var out []JournalRecord
	err = scanLog(data, func(rec record, line int, isLast bool) error {
		out = append(out, projectJournalRecord(line, rec))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
