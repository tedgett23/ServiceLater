package apps

import (
	"fmt"
	"strings"
)

// Problem is one thing wrong with a bundle, located by file.
type Problem struct {
	File    string
	Message string
}

// Problems collects every problem found, so an author sees them all at once instead of one per run.
type Problems []Problem

func (p *Problems) add(file, format string, args ...any) {
	*p = append(*p, Problem{File: file, Message: fmt.Sprintf(format, args...)})
}

func (p Problems) Error() string {
	lines := make([]string, len(p))
	for i, x := range p {
		if x.File != "" {
			lines[i] = x.File + ": " + x.Message
		} else {
			lines[i] = x.Message
		}
	}
	return strings.Join(lines, "\n")
}

// orNil returns nil when there is nothing to report, so callers can use the usual `if err != nil`.
func (p Problems) orNil() error {
	if len(p) == 0 {
		return nil
	}
	return p
}
