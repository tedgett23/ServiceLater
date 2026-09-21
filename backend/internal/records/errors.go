package records

// Error is an expected failure of a save, with a message a person can act on. Callers map the kind to their own
// idiom (an HTTP status, a job outcome). Anything that is not an *Error is a fault.
type Error struct {
	Kind Kind
	Msg  string
}

type Kind int

const (
	KindInvalid   Kind = iota // the change is not allowed as asked
	KindForbidden             // the actor may not do this
	KindConflict              // the ticket is not in a state that allows it
	KindBad                   // the request itself was malformed
)

func (e *Error) Error() string { return e.Msg }

func Invalid(msg string) error   { return &Error{KindInvalid, msg} }
func Forbidden(msg string) error { return &Error{KindForbidden, msg} }
func Conflict(msg string) error  { return &Error{KindConflict, msg} }
func Bad(msg string) error       { return &Error{KindBad, msg} }
