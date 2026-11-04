package eventlog

import "errors"

func (l *LightState) validateCheckpoint() error {
	s := &l.state
	if l.attachments == nil || l.asked == nil || l.resolved == nil || l.commands == nil {
		return errors.New("invalid checkpoint indexes")
	}
	if s.Head() == 0 {
		if s.created {
			return errors.New("invalid empty checkpoint")
		}
		return nil
	}
	if !s.created || s.model == "" || s.inputs == nil || s.requests == nil || s.turnIDs == nil || s.children == nil || s.backends == nil {
		return errors.New("invalid checkpoint state")
	}
	if s.turnAt < 0 || s.turnAt > len(s.history) {
		return errors.New("invalid checkpoint turn position")
	}
	return nil
}
