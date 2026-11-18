package lnd

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/bridgerpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// One quote at a time may start a channel from the on-chain pile. The mark
// stops a second quote from opening another toward the same wallet before
// the first is pending in the database. It expires so a failed attempt does
// not block that wallet forever.
const bridgeRecipientHold = 10 * time.Minute

var (
	bridgeRecipientMu      sync.Mutex
	bridgeRecipientOpening = map[string]time.Time{}
)

// ensureRecipientChannel opens one private channel from this node's on-chain
// balance toward an enrolled wallet, with the balance on this side.
//
// It runs only from a toBLAKE2b quote that already found no route. It does
// not dial a host out of an invoice. The peer must already be connected. A
// channel the wallet opened the other way does not count. Push is zero, the
// channel is not taproot, and another is not opened while one this node
// started is still confirming.
func (s *server) ensureRecipientChannel(ctx context.Context, dest string,
	dueMsat uint64) bool {

	bridgeRecipientMu.Lock()
	defer bridgeRecipientMu.Unlock()

	dest = strings.ToLower(strings.TrimSpace(dest))
	raw, err := hex.DecodeString(dest)
	if err != nil || len(raw) != 33 || (raw[0] != 2 && raw[0] != 3) {
		return false
	}
	pub, err := btcec.ParsePubKey(raw)
	if err != nil {
		return false
	}
	if s.identityECDH == nil || s.identityECDH.PubKey() == nil {
		return false
	}
	own := hex.EncodeToString(s.identityECDH.PubKey().SerializeCompressed())
	if strings.EqualFold(own, dest) {
		return false
	}

	st, ok := s.recipientView(dest, pub)
	if !ok {
		if at, exists := bridgeRecipientOpening[dest]; exists &&
			time.Since(at) < bridgeRecipientHold {

			return true
		}

		return false
	}
	if !st.Enrolled {
		delete(bridgeRecipientOpening, dest)

		return false
	}
	sats, waiting := bridgerpc.RecipientPlan(dueMsat, st)
	if waiting {
		delete(bridgeRecipientOpening, dest)

		return true
	}
	if bridgerpc.CoversDue(st.OpenLocalSats, dueMsat) {
		delete(bridgeRecipientOpening, dest)

		return false
	}
	if at, exists := bridgeRecipientOpening[dest]; exists &&
		time.Since(at) < bridgeRecipientHold {

		return true
	}
	if sats == 0 || ctx.Err() != nil {
		return false
	}
	if s.fundingMgr == nil || s.cc == nil || s.cc.FeeEstimator == nil {
		return false
	}
	feeRate, err := lnrpc.CalculateFeeRate(0, 0, 6, s.cc.FeeEstimator)
	if err != nil || feeRate == 0 {
		return false
	}

	fv := lnwire.NewRawFeatureVector(
		lnwire.StaticRemoteKeyRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
	)
	anchors := lnwire.ChannelType(*fv)
	bridgeRecipientOpening[dest] = time.Now()
	srvrLog.Infof("Bridge is opening a %d sat private channel toward %s",
		sats, dest)
	updates, errs := s.OpenChannel(&funding.InitFundingMsg{
		TargetPubkey:    pub,
		ChainHash:       s.cfg.ActiveNetParams.ChainHash,
		LocalFundingAmt: btcutil.Amount(sats),
		PushAmt:         0,
		FundingFeePerKw: feeRate,
		Private:         true,
		MinConfs:        1,
		ChannelType:     &anchors,
	})
	select {
	case err := <-errs:
		delete(bridgeRecipientOpening, dest)
		if err != nil {
			srvrLog.Infof("Bridge did not open a channel toward %s: %v",
				dest, err)
		}

		return false
	case <-updates:
		return true
	case <-ctx.Done():
		return true
	}
}

// recipientView reads enrollment, the peer, and the channels already open
// toward dest. ok is false when the channel list or the confirmed balance
// could not be read for a wallet this might open toward.
func (s *server) recipientView(dest string, pub *btcec.PublicKey) (
	bridgerpc.RecipientState, bool) {

	var st bridgerpc.RecipientState
	body, err := os.ReadFile(filepath.Join(s.cfg.LndDir, "bridge-enroll",
		"index.json"))
	if err == nil {
		st.Enrolled = bridgerpc.Enrolled(body, dest)
	}
	wire := string(pub.SerializeCompressed())
	s.mu.RLock()
	_, st.Connected = s.peersByPub[wire]
	s.mu.RUnlock()
	if !st.Enrolled || !st.Connected {
		return st, true
	}
	if s.chanStateDB == nil {
		return st, false
	}
	pending, err := s.chanStateDB.FetchPendingChannels()
	if err != nil {
		return st, false
	}
	for _, ch := range pending {
		if ch == nil || !sameRecipient(ch.IdentityPub, dest) || !ch.IsInitiator {
			continue
		}
		st.PendingOurChannel = true
	}
	open, err := s.chanStateDB.FetchAllOpenChannels()
	if err != nil {
		return st, false
	}
	var best int64
	for _, ch := range open {
		if ch == nil || !sameRecipient(ch.IdentityPub, dest) {
			continue
		}
		local := int64(ch.LocalCommitment.LocalBalance.ToSatoshis())
		spend := bridgerpc.SpendableLocal(local, int64(ch.Capacity))
		if spend > best {
			best = spend
		}
	}
	st.OpenLocalSats = best
	if s.cc == nil || s.cc.Wallet == nil {
		return st, false
	}
	bal, err := s.cc.Wallet.ConfirmedBalance(1, lnwallet.DefaultAccountName)
	if err != nil {
		// A channel already in progress can be reported without the
		// balance. Opening a new one cannot.
		if st.PendingOurChannel || best > 0 {
			return st, true
		}

		return st, false
	}
	st.ConfirmedSats = int64(bal)

	return st, true
}

func sameRecipient(pub *btcec.PublicKey, dest string) bool {
	if pub == nil {
		return false
	}

	return strings.EqualFold(hex.EncodeToString(pub.SerializeCompressed()), dest)
}
