// Package helperapi is the wire between snugbox, snugboxd and snugbox-helper:
// framing, the caller checks on the Unix socket, the bounds every request
// must keep, and a client. Blueprint section 5.4 sets the rules: one message
// type per fix, a size cap, bounds checked on every field, the caller's
// identity checked on every message, and no file descriptors accepted.
package helperapi

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Size caps. A request is small and parsed by root, so it gets a tight cap;
// an answer is parsed by the client and may list many containers.
const (
	MaxRequest  = 16 << 10
	MaxResponse = 4 << 20
)

// ErrTooLarge means a frame announced more bytes than its cap.
var ErrTooLarge = errors.New("helperapi: message larger than the limit")

// WriteFrame writes m as a uvarint byte count and the encoded message.
func WriteFrame(w io.Writer, m proto.Message, limit int) error {
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return fmt.Errorf("helperapi: %w", err)
	}
	if len(body) > limit {
		return ErrTooLarge
	}
	frame := binary.AppendUvarint(make([]byte, 0, len(body)+binary.MaxVarintLen32), uint64(len(body)))
	if _, err := w.Write(append(frame, body...)); err != nil {
		return fmt.Errorf("helperapi: %w", err)
	}
	return nil
}

// ReadFrame reads one frame into m. It refuses a frame over limit before
// reading its body, and any field m's schema doesn't have.
func ReadFrame(r *bufio.Reader, m proto.Message, limit int) error {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("helperapi: frame length: %w", err)
	}
	if limit < 0 || n > uint64(limit) {
		return ErrTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("helperapi: frame body: %w", err)
	}
	if err := proto.Unmarshal(body, m); err != nil {
		return fmt.Errorf("helperapi: %w", err)
	}
	if hasUnknown(m.ProtoReflect()) {
		return errors.New("helperapi: the message has fields this version does not know")
	}
	return nil
}

// hasUnknown reports whether m or any message inside it carries fields
// outside the schema, which a strict reader refuses.
func hasUnknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					found = hasUnknown(mv.Message())
					return !found
				})
			}
		case fd.IsList() && fd.Message() != nil:
			list := v.List()
			for i := range list.Len() {
				if hasUnknown(list.Get(i).Message()) {
					found = true
					break
				}
			}
		case fd.Message() != nil:
			found = hasUnknown(v.Message())
		}
		return !found
	})
	return found
}
