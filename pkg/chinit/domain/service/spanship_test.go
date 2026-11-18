// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

type fakeSpanLogReader struct {
	lastQuery port.SpanLogQuery
	spans     []model.RecordedSpan
	err       error
}

func (f *fakeSpanLogReader) ReadSpans(_ context.Context, query port.SpanLogQuery) ([]model.RecordedSpan, error) {
	f.lastQuery = query
	return f.spans, f.err
}

type fakeSpanSink struct {
	batches []port.SpanBatch
	err     error
}

func (f *fakeSpanSink) Export(_ context.Context, batch port.SpanBatch) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.batches = append(f.batches, batch)
	return len(batch.Spans), nil
}

type fixedSpanClock struct {
	now time.Time
}

func (c fixedSpanClock) Now() time.Time                  { return c.now }
func (c fixedSpanClock) Since(t time.Time) time.Duration { return c.now.Sub(t) }

var _ = Describe("SpanShipper", func() {
	It("skips keeper nodes without querying ClickHouse", func() {
		reader := &fakeSpanLogReader{err: errors.New("must not be called")}
		sink := &fakeSpanSink{}
		shipper := NewSpanShipper(SpanShipperDeps{Reader: reader, Sink: sink})

		res, err := shipper.Ship(context.Background(), SpanShipRequest{Role: model.RoleKeeper})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.SkippedReason).NotTo(BeEmpty())
		Expect(sink.batches).To(BeEmpty())
	})

	It("initializes lookback cursor on first run, exports batch, and persists JSON cursor only on success", func() {
		cursorFile := filepath.Join(GinkgoT().TempDir(), "span-ship.json")
		cursors := NewJSONSpanCursorStore(cursorFile)
		now := time.Unix(1_800_000_300, 0)

		reader := &fakeSpanLogReader{
			spans: []model.RecordedSpan{{
				Hostname:         "writer-0",
				TraceID:          "11111111-2222-3333-4444-555555555555",
				SpanID:           42,
				OperationName:    "query",
				StartTimeMicros:  1_800_000_100_000_000,
				FinishTimeMicros: 1_800_000_250_000_000,
			}},
		}
		sink := &fakeSpanSink{err: errors.New("collector unavailable")}
		shipper := NewSpanShipper(SpanShipperDeps{
			Reader:          reader,
			Cursors:         cursors,
			Sink:            sink,
			Clock:           fixedSpanClock{now: now},
			InitialLookback: 5 * time.Minute,
		})

		// Export failure must NOT advance the persisted cursor.
		_, err := shipper.Ship(context.Background(), SpanShipRequest{
			Cluster: "ch-int-dev", Role: model.RoleWriter, Hostname: "writer-0",
		})
		Expect(err).To(HaveOccurred())
		_, ok, loadErr := cursors.LoadCursor()
		Expect(loadErr).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(reader.lastQuery.AfterFinishMicros).To(Equal(uint64(1_800_000_000_000_000)))

		// Recovery advances the JSON cursor to the highest FinishTimeMicros.
		sink.err = nil
		res, err := shipper.Ship(context.Background(), SpanShipRequest{
			Cluster: "ch-int-dev", Role: model.RoleWriter, Hostname: "writer-0",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Exported).To(Equal(1))
		Expect(res.HighWaterMicros).To(Equal(uint64(1_800_000_250_000_000)))

		stored, ok, err := cursors.LoadCursor()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(stored.FinishTimeMicros).To(Equal(uint64(1_800_000_250_000_000)))
	})
})
