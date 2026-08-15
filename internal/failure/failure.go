package failure

import (
	"errors"
	"fmt"
)

type Kind string

const (
	Transient  Kind = "transient"
	Issue      Kind = "issue"
	Supervisor Kind = "supervisor"
)

type Error struct {
	Kind      Kind
	Operation string
	Err       error
}

func (e *Error) Error() string {
	if e.Operation == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %v", e.Operation, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func Wrap(kind Kind, operation string, err error) error {
	if err == nil {
		return nil
	}
	var classified *Error
	if errors.As(err, &classified) {
		return err
	}
	return &Error{Kind: kind, Operation: operation, Err: err}
}

func KindOf(err error) Kind {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Kind
	}
	return Supervisor
}
