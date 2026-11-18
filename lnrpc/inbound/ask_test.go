package inbound

import (
	"encoding/binary"
	"testing"
)

func askBytes(room uint64, txid byte) []byte {
	data := make([]byte, 40)
	binary.BigEndian.PutUint64(data[:8], room)
	for i := 8; i < 40; i++ {
		data[i] = txid
	}

	return data
}

func TestParseAndFee(t *testing.T) {
	ask, ok := Parse(askBytes(250_000, 0xab))
	if !ok || ask.Room != 250_000 || len(ask.Txid) != 64 {
		t.Fatalf("parse %+v %v", ask, ok)
	}
	if _, ok := Parse(askBytes(250_000, 0xab)[:39]); ok {
		t.Fatal("short ask was kept")
	}
	fee, ok := Fee(250_000)
	if !ok || fee != 3_500 {
		t.Fatalf("fee %d %v", fee, ok)
	}
	if _, ok := Fee(247_100); !ok {
		t.Fatal("247100 is a fee this service names")
	}
	fee, ok = Fee(247_100)
	if !ok || fee != 3_471 {
		t.Fatalf("247100 fee %d", fee)
	}
	if _, ok := Fee(RoomMax + 100); ok {
		t.Fatal("over the maximum was named")
	}
	if _, ok := Fee(19_900); ok {
		t.Fatal("under the minimum was named")
	}
}

func TestDecide(t *testing.T) {
	ask, ok := Parse(askBytes(250_000, 0x11))
	if !ok {
		t.Fatal("parse")
	}
	paid := View{FeeValue: 3_500, FeeConfs: 1, FeeScriptOK: true, Confirmed: 280_000}
	local, push, open := Decide(ask, paid)
	if !open || local != 270_000 || push != 20_000 {
		t.Fatalf("open local=%d push=%d open=%v", local, push, open)
	}

	short := paid
	short.Confirmed = 260_202
	if _, _, open := Decide(ask, short); open {
		t.Fatal("260202 cannot fund 250000")
	}

	unpaid := paid
	unpaid.FeeScriptOK = false
	if _, _, open := Decide(ask, unpaid); open {
		t.Fatal("a missing fee opened")
	}

	used := paid
	used.AlreadyUsed = true
	if _, _, open := Decide(ask, used); open {
		t.Fatal("a used fee opened again")
	}

	pending := paid
	pending.Pending = true
	if _, _, open := Decide(ask, pending); open {
		t.Fatal("a pending channel opened another")
	}

	have := paid
	have.HaveLocal = 250_000
	if _, _, open := Decide(ask, have); open {
		t.Fatal("an open channel of the same size opened another")
	}

	wide, ok := Parse(askBytes(RoomMax, 0x22))
	if !ok {
		t.Fatal("parse max")
	}
	funded := View{
		FeeValue:    1_000 + int64(RoomMax/100),
		FeeConfs:    6,
		FeeScriptOK: true,
		Confirmed:   CapacityMax + LeaveSats,
	}
	local, push, open = Decide(wide, funded)
	if !open || local != CapacityMax || push != PushSats {
		t.Fatalf("max local=%d push=%d open=%v", local, push, open)
	}
}
