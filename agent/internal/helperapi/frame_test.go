package helperapi

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

func swapRequest(size uint64) *helperv1.Request {
	return &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{
		Fix: &helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: &helperv1.SwapFix{SizeBytes: size, Compressor: helperv1.Compressor_COMPRESSOR_ZSTD}}},
	}}}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := swapRequest(2 * GiB)
	if err := WriteFrame(&buf, want, MaxRequest); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, &helperv1.Request{Kind: &helperv1.Request_List{List: &helperv1.ListRequest{}}}, MaxRequest); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	var got helperv1.Request
	if err := ReadFrame(r, &got, MaxRequest); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&got, want) {
		t.Errorf("read back %v, want %v", &got, want)
	}
	var second helperv1.Request
	if err := ReadFrame(r, &second, MaxRequest); err != nil || second.GetList() == nil {
		t.Errorf("second frame: %v, %v", &second, err)
	}
	if err := ReadFrame(r, &second, MaxRequest); !errors.Is(err, io.EOF) {
		t.Errorf("after the last frame: %v, want io.EOF", err)
	}
}

func TestFrameLimits(t *testing.T) {
	var buf bytes.Buffer
	big := &helperv1.Request{Kind: &helperv1.Request_Undo{Undo: &helperv1.UndoRequest{Reason: strings.Repeat("x", MaxRequest)}}}
	if err := WriteFrame(&buf, big, MaxRequest); !errors.Is(err, ErrTooLarge) {
		t.Errorf("writing an oversized request: %v, want ErrTooLarge", err)
	}

	// A frame that announces a huge body is refused from its length alone,
	// before anything is allocated for it.
	announce := binary.AppendUvarint(nil, 1<<40)
	var req helperv1.Request
	if err := ReadFrame(bufio.NewReader(bytes.NewReader(announce)), &req, MaxRequest); !errors.Is(err, ErrTooLarge) {
		t.Errorf("reading a frame that announces 1 TiB: %v, want ErrTooLarge", err)
	}

	short := append(binary.AppendUvarint(nil, 10), 1, 2, 3)
	if err := ReadFrame(bufio.NewReader(bytes.NewReader(short)), &req, MaxRequest); err == nil {
		t.Error("a frame shorter than its length was accepted")
	}
}

func TestUnknownFieldsAreRefused(t *testing.T) {
	body, err := proto.Marshal(swapRequest(GiB))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"at the top": protowire.AppendVarint(protowire.AppendTag(bytes.Clone(body), 99, protowire.VarintType), 1),
	}
	// An unknown field inside SwapFix: Request.apply(1) > ApplyRequest.fix(1)
	// > Fix.swap(1) > field 9.
	swap := protowire.AppendVarint(protowire.AppendTag(nil, 9, protowire.VarintType), 7)
	fix := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), swap)
	apply := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), fix)
	cases["deep inside"] = protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), apply)

	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			frame := append(binary.AppendUvarint(nil, uint64(len(b))), b...)
			var req helperv1.Request
			if err := ReadFrame(bufio.NewReader(bytes.NewReader(frame)), &req, MaxRequest); err == nil {
				t.Errorf("accepted %v with an unknown field", &req)
			}
		})
	}
}

func FuzzReadFrame(f *testing.F) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, swapRequest(GiB), MaxRequest); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte{0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})
	f.Fuzz(func(t *testing.T, data []byte) {
		var req helperv1.Request
		if err := ReadFrame(bufio.NewReader(bytes.NewReader(data)), &req, MaxRequest); err != nil {
			return
		}
		// Whatever parses must be safe to validate.
		_ = Validate(&req, Box{MemTotal: 2 * GiB})
	})
}
