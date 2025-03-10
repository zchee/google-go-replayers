// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package grpcreplay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	rpb "github.com/google/go-replayers/grpcreplay/proto/grpcreplay"
	ipb "github.com/google/go-replayers/grpcreplay/proto/intstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

var formats = []struct {
	name      string
	newWriter func(w io.Writer) writer
	newReader func(r io.Reader) reader
}{
	{
		"binary",
		func(w io.Writer) writer { return &binaryWriter{w} },
		func(r io.Reader) reader { return &binaryReader{r} },
	},
	{
		"text",
		func(w io.Writer) writer { return &textWriter{w} },
		func(r io.Reader) reader { return newTextReader(r, "") },
	},
}

func TestNewReader(t *testing.T) {
	for _, test := range []struct {
		data string
		want reflect.Type
	}{
		{binaryMagic, reflect.TypeOf(&binaryReader{})},
		{textMagic, reflect.TypeOf(&textReader{})},
	} {
		r := strings.NewReader(test.data)
		rr, err := newReader(r, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := reflect.TypeOf(rr); got != test.want {
			t.Errorf("%q: got %s, want %s", test.data, got, test.want)
		}
	}
}

func TestHeaderIO(t *testing.T) {
	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			want := []byte{1, 2, 3}
			w := format.newWriter(buf)
			r := format.newReader(buf)
			if err := w.writeHeader(want); err != nil {
				t.Fatal(err)
			}
			got, err := r.readHeader()
			if err != nil {
				t.Fatal(err)
			}
			if !cmp.Equal(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

func TestEntryIO(t *testing.T) {
	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			for i, want := range []*entry{
				{
					kind:     rpb.Entry_REQUEST,
					method:   "method",
					msg:      message{msg: &rpb.Entry{}},
					refIndex: 7,
				},
				{
					kind:     rpb.Entry_RESPONSE,
					method:   "method",
					msg:      message{err: status.Error(codes.NotFound, "not found")},
					refIndex: 8,
				},
				{
					kind:     rpb.Entry_RECV,
					method:   "method",
					msg:      message{err: io.EOF},
					refIndex: 3,
				},
			} {
				buf := &bytes.Buffer{}
				w := format.newWriter(buf)
				r := format.newReader(buf)
				if err := w.writeEntry(want); err != nil {
					t.Fatal(err)
				}
				got, err := r.readEntry()
				if err != nil {
					t.Fatal(err)
				}
				if !got.equal(want) {
					t.Errorf("#%d: got %v, want %v", i, got, want)
				}
			}
		})
	}
}

var initialState = []byte{1, 2, 3}

func TestRecord(t *testing.T) {
	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			buf := record(t, format.name, testService)
			r, err := newReader(buf, "")
			if err != nil {
				t.Fatal(err)
			}

			gotIstate, err := r.readHeader()
			if err != nil {
				t.Fatal(err)
			}
			if !cmp.Equal(gotIstate, initialState) {
				t.Fatalf("got %v, want %v", gotIstate, initialState)
			}
			item := &ipb.Item{Name: "a", Value: 1}
			wantEntries := []*entry{
				// Set
				{
					kind:   rpb.Entry_REQUEST,
					method: "/intstore.IntStore/Set",
					msg:    message{msg: item},
				},
				{
					kind:     rpb.Entry_RESPONSE,
					msg:      message{msg: &ipb.SetResponse{PrevValue: 0}},
					refIndex: 1,
				},
				// Get
				{
					kind:   rpb.Entry_REQUEST,
					method: "/intstore.IntStore/Get",
					msg:    message{msg: &ipb.GetRequest{Name: "a"}},
				},
				{
					kind:     rpb.Entry_RESPONSE,
					msg:      message{msg: item},
					refIndex: 3,
				},
				{
					kind:   rpb.Entry_REQUEST,
					method: "/intstore.IntStore/Get",
					msg:    message{msg: &ipb.GetRequest{Name: "x"}},
				},
				{
					kind:     rpb.Entry_RESPONSE,
					msg:      message{err: status.Error(codes.NotFound, `"x"`)},
					refIndex: 5,
				},
				// ListItems
				{ // entry #7
					kind:   rpb.Entry_CREATE_STREAM,
					method: "/intstore.IntStore/ListItems",
				},
				{
					kind:     rpb.Entry_SEND,
					msg:      message{msg: &ipb.ListItemsRequest{}},
					refIndex: 7,
				},
				{
					kind:     rpb.Entry_RECV,
					msg:      message{msg: item},
					refIndex: 7,
				},
				{
					kind:     rpb.Entry_RECV,
					msg:      message{err: io.EOF},
					refIndex: 7,
				},
				// SetStream
				{ // entry #11
					kind:   rpb.Entry_CREATE_STREAM,
					method: "/intstore.IntStore/SetStream",
				},
				{
					kind:     rpb.Entry_SEND,
					msg:      message{msg: &ipb.Item{Name: "b", Value: 2}},
					refIndex: 11,
				},
				{
					kind:     rpb.Entry_SEND,
					msg:      message{msg: &ipb.Item{Name: "c", Value: 3}},
					refIndex: 11,
				},
				{
					kind:     rpb.Entry_RECV,
					msg:      message{msg: &ipb.Summary{Count: 2}},
					refIndex: 11,
				},

				// StreamChat
				{ // entry #15
					kind:   rpb.Entry_CREATE_STREAM,
					method: "/intstore.IntStore/StreamChat",
				},
				{
					kind:     rpb.Entry_SEND,
					msg:      message{msg: &ipb.Item{Name: "d", Value: 4}},
					refIndex: 15,
				},
				{
					kind:     rpb.Entry_RECV,
					msg:      message{msg: &ipb.Item{Name: "d", Value: 4}},
					refIndex: 15,
				},
				{
					kind:     rpb.Entry_SEND,
					msg:      message{msg: &ipb.Item{Name: "e", Value: 5}},
					refIndex: 15,
				},
				{
					kind:     rpb.Entry_RECV,
					msg:      message{msg: &ipb.Item{Name: "e", Value: 5}},
					refIndex: 15,
				},
				{
					kind:     rpb.Entry_RECV,
					msg:      message{err: io.EOF},
					refIndex: 15,
				},
			}
			for i, w := range wantEntries {
				g, err := r.readEntry()
				if err != nil {
					t.Fatalf("#%d: %v", i+1, err)
				}
				if !g.equal(w) {
					t.Errorf("#%d:\ngot  %+v\nwant %+v", i+1, g, w)
				}
			}
			g, err := r.readEntry()
			if err != nil {
				t.Fatal(err)
			}
			if g != nil {
				t.Errorf("\ngot  %+v\nwant nil", g)
			}
		})
	}
}

func TestReplay(t *testing.T) {
	for _, format := range formats {
		buf := record(t, format.name, testService)
		replay(t, buf, testService)
	}
}

func record(t *testing.T, format string, run func(*testing.T, *grpc.ClientConn)) *bytes.Buffer {
	srv := newIntStoreServer()
	defer srv.stop()

	buf := &bytes.Buffer{}
	opts := &RecorderOptions{Initial: initialState}
	opts.Text = (format == "text")
	rec, err := NewRecorderWriter(buf, opts)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.Dial(srv.Addr,
		append([]grpc.DialOption{grpc.WithInsecure()}, rec.DialOptions()...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	run(t, conn)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	return buf
}

func replay(t *testing.T, buf *bytes.Buffer, run func(*testing.T, *grpc.ClientConn)) {
	rep, err := NewReplayerReader(buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	if got, want := rep.Initial(), initialState; !cmp.Equal(got, want, protocmp.Transform()) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Replay the test.
	conn, err := rep.Connection()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	run(t, conn)
}

func testService(t *testing.T, conn *grpc.ClientConn) {
	client := ipb.NewIntStoreClient(conn)
	ctx := context.Background()
	item := &ipb.Item{Name: "a", Value: 1}
	res, err := client.Set(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if res.PrevValue != 0 {
		t.Errorf("got %d, want 0", res.PrevValue)
	}
	got, err := client.Get(ctx, &ipb.GetRequest{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, item) {
		t.Errorf("got %v, want %v", got, item)
	}
	_, err = client.Get(ctx, &ipb.GetRequest{Name: "x"})
	if err == nil {
		t.Fatal("got nil, want error")
	}
	if _, ok := status.FromError(err); !ok {
		t.Errorf("got error type %T, want a grpc/status.Status", err)
	}

	gotItems := listItems(t, client, 0)
	compareLists(t, gotItems, []*ipb.Item{item})

	ssc, err := client.SetStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}

	for i, name := range []string{"b", "c"} {
		must(ssc.Send(&ipb.Item{Name: name, Value: int32(i + 2)}))
	}
	summary, err := ssc.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := summary.Count, int32(2); got != want {
		t.Fatalf("got %d, want %d", got, want)
	}

	chatc, err := client.StreamChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"d", "e"} {
		item := &ipb.Item{Name: name, Value: int32(i + 4)}
		must(chatc.Send(item))
		got, err := chatc.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got, item) {
			t.Errorf("got %v, want %v", got, item)
		}
	}
	must(chatc.CloseSend())
	if _, err := chatc.Recv(); err != io.EOF {
		t.Fatalf("got %v, want EOF", err)
	}
}

func listItems(t *testing.T, client ipb.IntStoreClient, greaterThan int) []*ipb.Item {
	t.Helper()
	lic, err := client.ListItems(context.Background(), &ipb.ListItemsRequest{GreaterThan: int32(greaterThan)})
	if err != nil {
		t.Fatal(err)
	}
	var items []*ipb.Item
	for i := 0; ; i++ {
		item, err := lic.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

func compareLists(t *testing.T, got, want []*ipb.Item) {
	t.Helper()
	diff := cmp.Diff(got, want, cmp.Comparer(proto.Equal), cmpopts.SortSlices(func(i1, i2 *ipb.Item) bool {
		return i1.Value < i2.Value
	}))
	if diff != "" {
		t.Error(diff)
	}
}

func TestRecorderBeforeFunc(t *testing.T) {
	var tests = []struct {
		name                           string
		msg, wantRespMsg, wantEntryMsg *ipb.Item
		f                              func(string, proto.Message) error
		wantErr                        bool
	}{
		{
			name:         "BeforeFunc should modify messages saved, but not alter what is sent/received to/from services",
			msg:          &ipb.Item{Name: "foo", Value: 1},
			wantEntryMsg: &ipb.Item{Name: "bar", Value: 2},
			wantRespMsg:  &ipb.Item{Name: "foo", Value: 1},
			f: func(method string, m proto.Message) error {
				// This callback only runs when Set is called.
				if !strings.HasSuffix(method, "Set") {
					return nil
				}
				if _, ok := m.(*ipb.Item); !ok {
					return nil
				}

				item := m.(*ipb.Item)
				item.Name = "bar"
				item.Value = 2
				return nil
			},
		},
		{
			name:        "BeforeFunc should not be able to alter returned responses",
			msg:         &ipb.Item{Name: "foo", Value: 1},
			wantRespMsg: &ipb.Item{Name: "foo", Value: 1},
			f: func(method string, m proto.Message) error {
				// This callback only runs when Get is called.
				if !strings.HasSuffix(method, "Get") {
					return nil
				}
				if _, ok := m.(*ipb.Item); !ok {
					return nil
				}

				item := m.(*ipb.Item)
				item.Value = 2
				return nil
			},
		},
		{
			name: "Errors should cause the RPC send to fail",
			msg:  &ipb.Item{},
			f: func(_ string, _ proto.Message) error {
				return errors.New("err")
			},
			wantErr: true,
		},
	}

	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			for _, tc := range tests {
				// Wrap test cases in a func so defers execute correctly.
				func() {
					srv := newIntStoreServer()
					defer srv.stop()

					opts := &RecorderOptions{BeforeWrite: tc.f}
					opts.Text = (format.name == "text")
					var b bytes.Buffer
					r, err := NewRecorderWriter(&b, opts)
					if err != nil {
						t.Error(err)
						return
					}
					ctx := context.Background()
					conn, err := grpc.DialContext(ctx, srv.Addr, append([]grpc.DialOption{grpc.WithInsecure()}, r.DialOptions()...)...)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()

					client := ipb.NewIntStoreClient(conn)
					_, err = client.Set(ctx, tc.msg)
					switch {
					case err != nil && !tc.wantErr:
						t.Error(err)
						return
					case err == nil && tc.wantErr:
						t.Errorf("got nil; want error")
						return
					case err != nil:
						// Error found as expected, don't check Get().
						return
					}

					if tc.wantRespMsg != nil {
						got, err := client.Get(ctx, &ipb.GetRequest{Name: tc.msg.GetName()})
						if err != nil {
							t.Error(err)
							return
						}
						if !cmp.Equal(got, tc.wantRespMsg, protocmp.Transform()) {
							t.Errorf("got %+v; want %+v", got, tc.wantRespMsg)
						}
					}

					r.Close()

					if tc.wantEntryMsg != nil {
						r, err := newReader(&b, "")
						if err != nil {
							t.Fatal(err)
						}
						_, _ = r.readHeader()
						e, err := r.readEntry()
						if err != nil {
							t.Error(err)
							return
						}
						got := e.msg.msg.(*ipb.Item)
						if !cmp.Equal(got, tc.wantEntryMsg, protocmp.Transform()) {
							t.Errorf("got %v; want %v", got, tc.wantEntryMsg)
						}
					}
				}()
			}
		})
	}
}

func TestReplayerBeforeFunc(t *testing.T) {
	var tests = []struct {
		name        string
		msg, reqMsg *ipb.Item
		f           func(string, proto.Message) error
		wantErr     bool
	}{
		{
			name:   "BeforeFunc should modify messages sent before they are passed to the replayer",
			msg:    &ipb.Item{Name: "foo", Value: 1},
			reqMsg: &ipb.Item{Name: "bar", Value: 1},
			f: func(method string, m proto.Message) error {
				item := m.(*ipb.Item)
				item.Name = "foo"
				return nil
			},
		},
		{
			name: "Errors should cause the RPC send to fail",
			msg:  &ipb.Item{},
			f: func(_ string, _ proto.Message) error {
				return errors.New("err")
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		// Wrap test cases in a func so defers execute correctly.
		func() {
			srv := newIntStoreServer()
			defer srv.stop()

			var b bytes.Buffer
			rec, err := NewRecorderWriter(&b, nil)
			if err != nil {
				t.Error(err)
				return
			}
			ctx := context.Background()
			conn, err := grpc.DialContext(ctx, srv.Addr, append([]grpc.DialOption{grpc.WithInsecure()}, rec.DialOptions()...)...)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()

			client := ipb.NewIntStoreClient(conn)
			_, err = client.Set(ctx, tc.msg)
			if err != nil {
				t.Error(err)
				return
			}
			rec.Close()

			rep, err := NewReplayerReader(&b, &ReplayerOptions{BeforeMatch: tc.f})
			if err != nil {
				t.Error(err)
				return
			}
			conn, err = rep.Connection()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()

			client = ipb.NewIntStoreClient(conn)
			_, err = client.Set(ctx, tc.reqMsg)
			switch {
			case err != nil && !tc.wantErr:
				t.Error(err)
			case err == nil && tc.wantErr:
				t.Errorf("got nil; want error")
			}
		}()
	}
}

func TestOutOfOrderStreamReplay(t *testing.T) {
	// Check that streams are matched by method and first request sent, if any.

	items := []*ipb.Item{
		{Name: "a", Value: 1},
		{Name: "b", Value: 2},
		{Name: "c", Value: 3},
	}
	run := func(t *testing.T, conn *grpc.ClientConn, arg1, arg2 int) {
		client := ipb.NewIntStoreClient(conn)
		ctx := context.Background()
		// Set some items.
		for _, item := range items {
			_, err := client.Set(ctx, item)
			if err != nil {
				t.Fatal(err)
			}
		}
		// List them twice, with different requests.
		compareLists(t, listItems(t, client, arg1), items[arg1:])
		compareLists(t, listItems(t, client, arg2), items[arg2:])
	}

	srv := newIntStoreServer()
	defer srv.stop()

	// Replay in the same order.
	buf := record(t, "binary", func(t *testing.T, conn *grpc.ClientConn) { run(t, conn, 1, 2) })
	replay(t, buf, func(t *testing.T, conn *grpc.ClientConn) { run(t, conn, 1, 2) })

	// Replay in a different order.
	buf = record(t, "binary", func(t *testing.T, conn *grpc.ClientConn) { run(t, conn, 1, 2) })
	replay(t, buf, func(t *testing.T, conn *grpc.ClientConn) { run(t, conn, 2, 1) })
}

func TestSetInitial(t *testing.T) {
	srv := newIntStoreServer()
	defer srv.stop()

	buf := &bytes.Buffer{}
	rec, err := NewRecorderWriter(buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec.SetInitial(initialState)
	conn, err := grpc.Dial(srv.Addr,
		append([]grpc.DialOption{grpc.WithInsecure()}, rec.DialOptions()...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := ipb.NewIntStoreClient(conn)
	ctx := context.Background()
	if _, err := client.Set(ctx, &ipb.Item{Name: "a", Value: 1}); err != nil {
		t.Fatal(err)
	}

	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	rep, err := NewReplayerReader(buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rep.Initial(), initialState; !bytes.Equal(got, want) {
		t.Errorf("got initial state %q, want %q", got, want)
	}

}
