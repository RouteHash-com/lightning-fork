package bridgerpc

import (
	"strings"
	"testing"
)

func TestRecipientPlanOpensOnlyWhenThePoolMust(t *testing.T) {
	const due = 1_000_000 // 1,000 sats
	ready := RecipientState{
		Enrolled:      true,
		Connected:     true,
		ConfirmedSats: 31_000,
	}
	sats, waiting := RecipientPlan(due, ready)
	if waiting || sats != 21_000 {
		t.Fatalf("open plan = %d waiting %v, want 21000", sats, waiting)
	}

	short := ready
	short.ConfirmedSats = 30_999
	if sats, waiting := RecipientPlan(due, short); sats != 0 || waiting {
		t.Fatalf("short confirmed opened %d waiting %v", sats, waiting)
	}

	wrongWay := ready
	wrongWay.OpenLocalSats = 0
	if sats, waiting := RecipientPlan(due, wrongWay); sats != 21_000 || waiting {
		t.Fatalf("a channel the wallet opened should still be opened toward, got %d %v",
			sats, waiting)
	}

	covers := ready
	covers.OpenLocalSats = 1_000
	if sats, waiting := RecipientPlan(due, covers); sats != 0 || waiting {
		t.Fatalf("spendable local should not open another, got %d %v", sats, waiting)
	}

	pending := ready
	pending.PendingOurChannel = true
	if sats, waiting := RecipientPlan(due, pending); sats != 0 || !waiting {
		t.Fatalf("pending initiator = %d waiting %v", sats, waiting)
	}

	offline := ready
	offline.Connected = false
	if sats, waiting := RecipientPlan(due, offline); sats != 0 || waiting {
		t.Fatalf("offline peer opened %d %v", sats, waiting)
	}

	stranger := ready
	stranger.Enrolled = false
	if sats, waiting := RecipientPlan(due, stranger); sats != 0 || waiting {
		t.Fatalf("unenrolled peer opened %d %v", sats, waiting)
	}

	if sats, waiting := RecipientPlan(0, ready); sats != 0 || waiting {
		t.Fatalf("zero invoice opened %d %v", sats, waiting)
	}
	if sats, waiting := RecipientPlan(20_000_000*1000, ready); sats != 0 || waiting {
		t.Fatalf("oversized invoice opened %d %v", sats, waiting)
	}

	// 550,000 sats needs 570,000 in the channel and 10,000 left on chain.
	big := RecipientState{Enrolled: true, Connected: true, ConfirmedSats: 580_000}
	sats, waiting = RecipientPlan(550_000_000, big)
	if waiting || sats != 570_000 {
		t.Fatalf("large open = %d waiting %v, want 570000", sats, waiting)
	}
}

func TestRecipientSpendableReserve(t *testing.T) {
	if got := SpendableLocal(0, 21_000); got != 0 {
		t.Fatalf("empty local %d", got)
	}
	if got := SpendableLocal(21_000, 21_000); got != 20_790 {
		t.Fatalf("reserve %d, want 20790", got)
	}
	if !CoversDue(1_000, 1_000_000) {
		t.Fatal("1000 spendable sats should cover a 1000 sat invoice")
	}
	if CoversDue(999, 1_000_000) {
		t.Fatal("999 sats should not cover 1000")
	}
}

func TestRecipientEnrolledReadsTheIndexOnly(t *testing.T) {
	bob := "02" + strings.Repeat("ab", 32)
	body := []byte(`[{"pubkey":"` + bob + `","root_key_id":"1","created":1}]` + "\n")
	if !Enrolled(body, strings.ToUpper(bob)) {
		t.Fatal("enrolled pubkey was missed")
	}
	if Enrolled(body, "03"+strings.Repeat("cd", 32)) {
		t.Fatal("a missing pubkey was enrolled")
	}
	if Enrolled([]byte(`not json`), bob) {
		t.Fatal("broken index enrolled someone")
	}
	if Enrolled([]byte(`[{"pubkey":"`+bob+`","code":"secret"}]`), "02"+strings.Repeat("11", 32)) {
		t.Fatal("another row's pubkey matched")
	}
}

func TestPublicPeerPrefersClearnetAndSkipsPools(t *testing.T) {
	id := "02" + strings.Repeat("11", 32)
	clear := id + "@203.0.113.9:9735"
	onion := id + "@" + strings.Repeat("a", 56) + ".onion:9735"
	got := choosePublicPeer(id, []string{
		id + "@10.59.254.119:9735",
		onion,
		clear,
		id + "@192.168.0.119:9735",
	})
	if got != clear {
		t.Fatalf("public peer %q, want %q", got, clear)
	}
	if got := choosePublicPeer(id, []string{onion, id + "@127.0.0.1:9735"}); got != onion {
		t.Fatalf("onion fallback %q", got)
	}
	if got := choosePublicPeer(id, []string{id + "@10.59.254.119:9735"}); got != "" {
		t.Fatalf("private address published %q", got)
	}
	if got := choosePublicPeer(eclairPoolKey, []string{eclairPoolKey + "@165.245.235.36:39735"}); got != "" {
		t.Fatalf("eclair key published %q", got)
	}
	if got := choosePublicPeer(xbtPoolKey, []string{xbtPoolKey + "@165.245.235.36:9744"}); got != "" {
		t.Fatalf("xbt pool key published %q", got)
	}
	if got := choosePublicPeer(id, []string{id + "@localhost:9735", id + "@node.local:9735"}); got != "" {
		t.Fatalf("local name published %q", got)
	}
	if got := choosePublicPeer(id, nil); got != "" {
		t.Fatalf("empty uri list published %q", got)
	}
	host := choosePublicPeer(strings.ToUpper(id), []string{
		strings.ToUpper(id) + "@ln.example.com:9735",
	})
	if host != id+"@ln.example.com:9735" {
		t.Fatalf("hostname %q", host)
	}
}

func TestRecipientOpeningSentenceStaysARefusal(t *testing.T) {
	if !strings.HasPrefix(RecipientOpeningSentence, "no_route: ") {
		t.Fatalf("sentence %q", RecipientOpeningSentence)
	}
	if strings.Contains(RecipientOpeningSentence, "The payment was not sent") {
		t.Fatal("the wallet adds that line itself")
	}
	if strings.Contains(strings.ToLower(RecipientOpeningSentence), "macaroon") {
		t.Fatal("sentence names a credential")
	}
	rest := strings.TrimPrefix(RecipientOpeningSentence, "no_route: ")
	if rest == "" || len(rest) > 240 {
		t.Fatalf("payer sentence %q", rest)
	}
}
