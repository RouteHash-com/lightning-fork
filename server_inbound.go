package lnd

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/inbound"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// One same-chain request at a time may claim a fee payment. The file keeps
// the claim across a restart so the same payment cannot open a second channel.
var inboundClaimMu sync.Mutex

// openSameChainInbound opens one private channel from this node's on-chain
// balance toward the peer that asked. Push is 20,000 sats. The rest stays on
// this side. A missing fee, a short balance, or a channel that is already
// large enough does not open one. This is not the bridge.
func (s *server) openSameChainInbound(peer [33]byte, data []byte) {
	ask, ok := inbound.Parse(data)
	if !ok {
		srvrLog.Infof("Same-chain channel was not opened: the request could not be read")

		return
	}
	if s.cc == nil || s.cc.Wallet == nil || s.fundingMgr == nil ||
		s.cc.FeeEstimator == nil || s.chanStateDB == nil {

		srvrLog.Infof("Same-chain channel was not opened: the wallet is not ready")

		return
	}
	view, ok := s.inboundView(peer, ask)
	if !ok {
		srvrLog.Infof("Same-chain channel for %d sats was not opened: the channels could not be read",
			ask.Room)

		return
	}
	local, push, open := inbound.Decide(ask, view)
	if !open {
		srvrLog.Infof("Same-chain channel for %d sats was not opened", ask.Room)

		return
	}
	if !s.markInboundClaim(ask.Txid) {
		srvrLog.Infof("Same-chain channel for %d sats was not opened: that fee was already used",
			ask.Room)

		return
	}
	pub, err := btcec.ParsePubKey(peer[:])
	if err != nil {
		s.unmarkInboundClaim(ask.Txid)

		return
	}
	feeRate, err := lnrpc.CalculateFeeRate(0, 0, 6, s.cc.FeeEstimator)
	if err != nil || feeRate == 0 {
		s.unmarkInboundClaim(ask.Txid)
		srvrLog.Infof("Same-chain channel for %d sats was not opened: no fee rate", ask.Room)

		return
	}
	srvrLog.Infof("Opening a %d sat private channel for a same-chain payment, push %d",
		local, push)
	updates, errs := s.OpenChannel(&funding.InitFundingMsg{
		TargetPubkey:    pub,
		ChainHash:       s.cfg.ActiveNetParams.ChainHash,
		LocalFundingAmt: btcutil.Amount(local),
		PushAmt:         lnwire.NewMSatFromSatoshis(btcutil.Amount(push)),
		FundingFeePerKw: feeRate,
		Private:         true,
		MinConfs:        1,
	})
	select {
	case err := <-errs:
		if err != nil {
			s.unmarkInboundClaim(ask.Txid)
			srvrLog.Infof("Same-chain channel for %d sats was not opened: %v",
				ask.Room, err)
		}
	case <-updates:
	}
}

// inboundView reads the fee payment, the balance, and the channels already
// open toward peer. ok is false when the channel list could not be read.
func (s *server) inboundView(peer [33]byte, ask inbound.Ask) (inbound.View, bool) {
	var view inbound.View
	view.AlreadyUsed = s.inboundClaimed(ask.Txid)
	script, err := inboundFeeScript()
	if err != nil {
		return view, true
	}
	hash, err := chainhash.NewHashFromStr(ask.Txid)
	if err == nil {
		detail, err := s.cc.Wallet.GetTransactionDetails(hash)
		if err == nil && detail != nil {
			view.FeeConfs = detail.NumConfirmations
			for _, out := range detail.OutputDetails {
				if outputPaysFee(out, script, ask) {
					view.FeeScriptOK = true
					view.FeeValue = int64(out.Value)
					break
				}
			}
		}
	}
	dest := hex.EncodeToString(peer[:])
	pending, err := s.chanStateDB.FetchPendingChannels()
	if err != nil {
		return view, false
	}
	for _, ch := range pending {
		if ch == nil || !ch.IsInitiator || !sameInboundPeer(ch.IdentityPub, dest) {
			continue
		}
		view.Pending = true
	}
	open, err := s.chanStateDB.FetchAllOpenChannels()
	if err != nil {
		return view, false
	}
	for _, ch := range open {
		if ch == nil || !sameInboundPeer(ch.IdentityPub, dest) {
			continue
		}
		local := int64(ch.LocalCommitment.LocalBalance.ToSatoshis())
		if local > view.HaveLocal {
			view.HaveLocal = local
		}
	}
	bal, err := s.cc.Wallet.ConfirmedBalance(1, lnwallet.DefaultAccountName)
	if err != nil {
		return view, false
	}
	view.Confirmed = int64(bal)

	return view, true
}

func outputPaysFee(out lnwallet.OutputDetail, script []byte, ask inbound.Ask) bool {
	fee, ok := inbound.Fee(ask.Room)
	if !ok || !out.IsOurAddress || int64(out.Value) != fee {
		return false
	}
	if bytes.Equal(out.PkScript, script) {
		return true
	}
	for _, addr := range out.Addresses {
		if addr != nil && addr.String() == inbound.FeeAddress {
			return true
		}
	}

	return false
}

func inboundFeeScript() ([]byte, error) {
	addr, err := btcutil.DecodeAddress(inbound.FeeAddress, &chaincfg.MainNetParams)
	if err != nil {
		return nil, err
	}

	return txscript.PayToAddrScript(addr)
}

func sameInboundPeer(pub *btcec.PublicKey, dest string) bool {
	if pub == nil {
		return false
	}

	return strings.EqualFold(hex.EncodeToString(pub.SerializeCompressed()), dest)
}

func (s *server) inboundClaimPath() string {
	return filepath.Join(s.cfg.LndDir, "inbound-claims.txt")
}

func (s *server) inboundClaimed(txid string) bool {
	inboundClaimMu.Lock()
	defer inboundClaimMu.Unlock()

	_, ok := s.readInboundClaims()[txid]

	return ok
}

func (s *server) markInboundClaim(txid string) bool {
	inboundClaimMu.Lock()
	defer inboundClaimMu.Unlock()
	claims := s.readInboundClaims()
	if _, ok := claims[txid]; ok {
		return false
	}
	claims[txid] = struct{}{}

	return s.writeInboundClaims(claims) == nil
}

func (s *server) unmarkInboundClaim(txid string) {
	inboundClaimMu.Lock()
	defer inboundClaimMu.Unlock()
	claims := s.readInboundClaims()
	delete(claims, txid)
	_ = s.writeInboundClaims(claims)
}

func (s *server) readInboundClaims() map[string]struct{} {
	out := map[string]struct{}{}
	body, err := os.ReadFile(s.inboundClaimPath())
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.ToLower(line))
		if len(line) == 64 {
			out[line] = struct{}{}
		}
	}

	return out
}

func (s *server) writeInboundClaims(claims map[string]struct{}) error {
	var body strings.Builder
	for txid := range claims {
		body.WriteString(txid)
		body.WriteByte('\n')
	}

	return os.WriteFile(s.inboundClaimPath(), []byte(body.String()), 0o600)
}

// noteInbound delivers a same-chain request without blocking the peer read.
func noteInbound(msg *lnwire.Custom) (data []byte, ok bool) {
	if msg == nil || uint16(msg.Type) != inbound.MessageType || len(msg.Data) != 40 {
		return nil, false
	}

	return append([]byte(nil), msg.Data...), true
}
