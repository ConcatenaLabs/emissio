package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var txidRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// chainCheck asks the testnet explorer whether a submitted txid exists and is
// confirmed. It is advisory: submissions are still human-reviewed, and an
// unreachable explorer must never block a submission.
func (a *App) chainCheck(txid string) string {
	if a.cfg.EsploraURL == "" {
		return "no explorer configured; manual review"
	}
	if !txidRe.MatchString(txid) {
		return "not a valid txid (must be 64 hex characters)"
	}
	client := &http.Client{Timeout: 6 * time.Second}
	url := strings.TrimRight(a.cfg.EsploraURL, "/") + "/tx/" + strings.ToLower(txid)
	resp, err := client.Get(url)
	if err != nil {
		return "explorer unreachable; manual review"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "txid NOT FOUND on the testnet"
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("explorer returned HTTP %d; manual review", resp.StatusCode)
	}
	var tx struct {
		Txid   string `json:"txid"`
		Status struct {
			Confirmed   bool  `json:"confirmed"`
			BlockHeight int64 `json:"block_height"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tx); err != nil {
		return "explorer response unreadable; manual review"
	}
	if tx.Status.Confirmed {
		return fmt.Sprintf("found on-chain, confirmed at height %d", tx.Status.BlockHeight)
	}
	return "found in the mempool, not yet confirmed"
}
