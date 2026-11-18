// Package inbound is a same-chain request for one private channel.
//
// The wallet that paid the fee asks over the peer connection. This service
// opens from its own on-chain balance toward that peer. It is not the bridge,
// it does not read an enrollment file, and it does not push the whole channel.
package inbound

import "encoding/hex"

const (
	// MessageType is the custom peer message for one same-chain open.
	// BOLT 01 custom types start at 32768.
	MessageType uint16 = 40100

	// FeeAddress receives the fee that names the amount others can send.
	// The channel capital is not that payment.
	FeeAddress = "bc1qeksdw883mnz0yr62ptgcd3d8x5nzg4afa5lvhk"

	RoomMin     uint64 = 20_000
	RoomMax     uint64 = 999_980_000
	PushSats    int64  = 20_000
	LeaveSats   int64  = 10_000
	CapacityMax int64  = 1_000_000_000
)

// Ask is one request from a connected wallet.
//
// Room is the amount that stays on this side, so the other wallet can receive
// it. Txid is the fee payment, in the 64-character form a wallet displays.
type Ask struct {
	Room uint64
	Txid string
}

// View is what this node already has for one ask.
//
// FeeValue is the output that paid FeeAddress. FeeScriptOK is false when that
// output was not in the named payment. HaveLocal is the most this node can
// already send that peer. Confirmed is the on-chain balance that can fund a
// new channel.
type View struct {
	Pending      bool
	HaveLocal    int64
	Confirmed    int64
	FeeValue     int64
	FeeConfs     int32
	FeeScriptOK  bool
	AlreadyUsed  bool
}

// Parse reads an 8-byte amount and a 32-byte payment id.
func Parse(data []byte) (Ask, bool) {
	if len(data) != 40 {
		return Ask{}, false
	}
	var room uint64
	for i := 0; i < 8; i++ {
		room = (room << 8) | uint64(data[i])
	}

	return Ask{Room: room, Txid: hex.EncodeToString(data[8:])}, true
}

// Fee is 1,000 sats plus 1 percent of room. Any other amount is not a request
// this service opens.
func Fee(room uint64) (int64, bool) {
	if room < RoomMin || room > RoomMax || room%100 != 0 {
		return 0, false
	}
	local := int64(room) + PushSats
	if local > CapacityMax || local-PushSats < PushSats {
		return 0, false
	}

	return 1_000 + int64(room/100), true
}

// Decide reports the channel to open, or not to open one.
//
// local is the full channel. push stays on the other wallet. The rest stays
// here. A short balance, a missing fee, a fee already used, or a channel
// already large enough is not an open.
func Decide(ask Ask, view View) (local int64, push int64, open bool) {
	fee, ok := Fee(ask.Room)
	if !ok || view.AlreadyUsed || view.Pending || !view.FeeScriptOK || view.FeeConfs < 1 {
		return 0, 0, false
	}
	if view.FeeValue != fee {
		return 0, 0, false
	}
	if view.HaveLocal >= int64(ask.Room) {
		return 0, 0, false
	}
	local = int64(ask.Room) + PushSats
	if view.Confirmed < local+LeaveSats {
		return 0, 0, false
	}

	return local, PushSats, true
}
