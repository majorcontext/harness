package turn

// FinishError carries the terminal reason of a failed model response.
type FinishError struct {
	Reason string
	Err    error
}

func (e *FinishError) Error() string { return e.Err.Error() }

func (e *FinishError) Unwrap() error { return e.Err }
