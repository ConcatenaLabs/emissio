package main

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A receive code as the web wallet writes it (pay-code.js buildPayCode): upper
// case, its Sequentia request short. The expansion must be the request the
// wallet's own expandRequest gives.
const testCode = "BITCOIN:TB1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KXPJZSX?ark=TARK1PNGLPCR3ZQQPQRP6TLZTDFMCHLYN76TQVXHG8XGUSPYMP8HG4F0JF445AQ3YZ4P3ZQYPEJS78MQ2CTZC8RZMDCCLKA7J4FE273DLXS87UD70ZV3KXDZVL9NQ3YWVYE&sequentia=SQR1QYXMDAX9JG7W32MVZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYGJYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3ZYG3NXVENXVENXVENXVENXVENXVENXVENXVENXVENXVENXVENXVENXVFUW66F6GQZ552488D9U6NQE2NHVMKDVKAVPSGDZ4CHMHFQ3ZZYHFV0F5ZTNHSQQQQQQP6ME52SM3WCKG"
const testOperator = "b6f4c5924ad81947658cf85bb93d9267efc54c7fcb2e4577e8c954c68fa087bb"
const testGenesis = "3ce8ab6c8f9836c0cebca304f3cbed3e824e6c53f4cacd2822f6f8441eed1a3e"
const testExpanded = `{"kind":"request","genesis_hash":"3ce8ab6c8f9836c0cebca304f3cbed3e824e6c53f4cacd2822f6f8441eed1a3e","operator":"b6f4c5924ad81947658cf85bb93d9267efc54c7fcb2e4577e8c954c68fa087bb","owner":"1111111111111111111111111111111111111111111111111111111111111111","owner_nonce":"2222222222222222222222222222222222222222222222222222222222222222","mailbox":"3333333333333333333333333333333333333333333333333333333333333333","exit_delay_units":5063,"until":1800000000,"asset":"2a515539da5e6a60caa7766ecd65bac0c10d15717ddd2088844ba58f4d04b9de","value":"123456789","template":"vtxo-2"}`

func TestReadReceiveCode(t *testing.T) {
	c, ok := readReceiveCode(testCode)
	if !ok || c.Address != "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx" || !strings.HasPrefix(c.Ark, "tark1") || !strings.HasPrefix(c.Sequentia, "sqr1") {
		t.Fatalf("read %+v %v", c, ok)
	}
	if _, ok := readReceiveCode("tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"); ok {
		t.Fatal("a plain address read as a code")
	}
	if got := addressOf(" " + testCode + " "); got != "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx" {
		t.Fatalf("addressOf = %q", got)
	}
	if got := addressOf(" tb1qabc "); got != "tb1qabc" {
		t.Fatalf("addressOf plain = %q", got)
	}
	addr, ark, req, _ := codeParts(testCode)
	if addr == "" || ark == "" || !strings.HasPrefix(req, "sqr1") {
		t.Fatalf("codeParts %q %q %q", addr, ark, req)
	}
}

func TestExpandRequest(t *testing.T) {
	c, _ := readReceiveCode(testCode)
	got, err := expandRequest(c.Sequentia, testOperator, testGenesis)
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimPrefix(got, "arca:"))
	if err != nil || !strings.HasPrefix(got, "arca:") {
		t.Fatalf("not the library's spelling: %q", got)
	}
	var have, want map[string]any
	json.Unmarshal(b, &have)
	json.Unmarshal([]byte(testExpanded), &want)
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("expanded\n%v\nwant\n%v", have, want)
	}
	if d, err := decodeRequest(got); err != nil || d.Owner != strings.Repeat("11", 32) {
		t.Fatalf("decodeRequest: %v %+v", err, d)
	}
	if _, err := expandRequest(c.Sequentia, strings.Repeat("ab", 32), testGenesis); err == nil || !strings.Contains(err.Error(), "another server") {
		t.Fatalf("another operator: %v", err)
	}
	if _, err := expandRequest(c.Sequentia, testOperator, strings.Repeat("ab", 32)); err == nil || !strings.Contains(err.Error(), "another chain") {
		t.Fatalf("another chain: %v", err)
	}
	if s, err := expandRequest("arca:7b7d", testOperator, testGenesis); err != nil || s != "arca:7b7d" {
		t.Fatalf("library spelling kept: %q %v", s, err)
	}
}
