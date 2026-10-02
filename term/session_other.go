//go:build !unix

package term

import (
	"context"
	"os"
)

// Session is unsupported on this platform (Windows/ConPTY is out of v0).
type Session struct {
	spec                        Spec
	childIn, childOut, childErr *os.File
	cttyFD                      int
}

func Open(spec Spec) (*Session, error)                       { return nil, ErrUnsupported }
func (s *Session) CloseChildEnds() error                     { return ErrUnsupported }
func (s *Session) Resize(cols, rows uint16) error            { return ErrUnsupported }
func (s *Session) Done(ctx context.Context) (Summary, error) { return Summary{}, ErrUnsupported }
func (s *Session) Close() error                              { return nil }
func (s *Session) Screen() string                            { return "" }
func (s *Session) Updates() <-chan struct{}                  { return nil }
func (s *Session) LiveDropped() int64                        { return 0 }
