package flowstats

import (
	"errors"
	"testing"
	"time"
)

func TestTrackerLossOutageRTT(t *testing.T) {
	tr := New(100*time.Millisecond, time.Second)
	t0 := time.Unix(1_000_000, 0)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * 100 * time.Millisecond) }
	// 20 packets; 5..9 are lost (a 500 ms outage); 3 is answered twice.
	for i := 0; i < 20; i++ {
		tr.Sent(uint64(i), at(i))
		if i < 5 || i >= 10 {
			tr.Expire(at(i), false)
			rtt := time.Duration(1+i%3) * time.Millisecond
			if got := tr.Received(uint64(i), at(i).Add(rtt), time.Time{}, ""); got != ResultReply {
				t.Fatalf("seq %d: %s", i, got)
			}
		}
		if i == 9 {
			tr.Expire(at(9).Add(time.Second), false) // 5..9 time out
		}
	}
	if got := tr.Received(3, at(20), time.Time{}, ""); got != ResultDup {
		t.Fatalf("dup: %s", got)
	}
	tr.Sent(20, at(20))
	tr.Expire(at(20).Add(2*time.Second), false)
	if got := tr.Received(20, at(20).Add(3*time.Second), time.Time{}, ""); got != ResultLate {
		t.Fatalf("late: %s", got)
	}
	tr.TxError(21, at(21), errors.New("boom"))
	tr.Sent(22, at(22)) // recorded first, then the send fails
	tr.TxError(22, at(22), errors.New("boom"))

	st := tr.Snapshot(at(25), 60)
	if st.Tx != 23 || st.Rx != 15 || st.Lost != 8 || st.Dup != 1 || st.Late != 1 || st.TxErrors != 2 || st.Pending != 0 {
		t.Fatalf("counts %+v", st)
	}
	if st.Outages != 1 || st.OutageMaxMs != 500 || st.OutageLastMs != 500 || st.OutageCurrentMs != 300 {
		t.Fatalf("outage %+v", st)
	}
	if st.RTT == nil || st.RTT.Minus != 1000 || st.RTT.Maxus != 3000 || st.RTT.Jitterus == 0 {
		t.Fatalf("rtt %+v", st.RTT)
	}
	// Buckets are by send second: second 0 holds packets 0-9 (5 lost).
	if len(st.Timeline) != 3 || st.Timeline[0].Tx != 10 || st.Timeline[0].Rx != 5 || st.Timeline[0].Lost != 5 || st.Timeline[1].Rx != 10 {
		t.Fatalf("timeline %+v", st.Timeline)
	}
	if n := len(st.Recent); n == 0 || st.Recent[n-1].Result != ResultTxError {
		t.Fatalf("recent %+v", st.Recent)
	}
}

func TestTrackerErrorsAndForce(t *testing.T) {
	tr := New(time.Second, time.Second)
	now := time.Unix(2_000_000, 0)
	tr.Sent(1, now)
	tr.Sent(2, now)
	tr.ErrorReceived(1, now.Add(time.Millisecond), "unreachable")
	tr.Expire(now.Add(10*time.Millisecond), true)
	st := tr.Snapshot(now, 10)
	if st.Errors != 1 || st.Lost != 2 || st.Pending != 0 || st.OutageCurrentMs != 2000 {
		t.Fatalf("%+v", st)
	}
}

func TestTrackerNoReplyOutage(t *testing.T) {
	tr := New(10*time.Millisecond, time.Second)
	now := time.Unix(3_000_000, 0)
	tr.SentNoReply(now)
	for i := uint64(1); i <= 5; i++ {
		tr.TxError(i, now, errors.New("port down"))
	}
	if st := tr.Snapshot(now, 10); st.OutageCurrentMs != 50 {
		t.Fatalf("during %+v", st)
	}
	tr.SentNoReply(now)
	st := tr.Snapshot(now, 10)
	if st.OutageCurrentMs != 0 || st.Outages != 1 || st.OutageMaxMs != 50 || st.Lost != 5 || st.Tx != 7 {
		t.Fatalf("after %+v", st)
	}
}
