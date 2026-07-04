package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Telegram account age estimation. Telegram assigns user IDs roughly
// monotonically, so a numeric ID maps to an approximate creation date. The
// anchor table below is the community dataset used by the various
// "creation date" bots (ID, unix milliseconds); we interpolate linearly
// between anchors. Estimates are advisory and shown to the reviewer.
type tgAnchor struct {
	ID int64
	MS int64
}

var tgAnchors = func() []tgAnchor {
	a := []tgAnchor{
		{2768409, 1383264000000}, {7679610, 1388448000000}, {11538514, 1391212000000},
		{15835244, 1392940000000}, {23646077, 1393459000000}, {38015510, 1393632000000},
		{44634663, 1399334000000}, {46145305, 1400198000000}, {54845238, 1411257000000},
		{63263518, 1414454000000}, {101260938, 1425600000000}, {101323197, 1426204000000},
		{111220210, 1429574000000}, {103258382, 1432771000000}, {103151531, 1433376000000},
		{116812045, 1437696000000}, {122600695, 1437782000000}, {109393468, 1439078000000},
		{112594714, 1439683000000}, {124872445, 1439856000000}, {130029930, 1441324000000},
		{125828524, 1444003000000}, {133909606, 1444176000000}, {157242073, 1446768000000},
		{143445125, 1448928000000}, {148670295, 1452211000000}, {152079341, 1453420000000},
		{171295414, 1457481000000}, {181783990, 1460246000000}, {222021233, 1465344000000},
		{225034354, 1466208000000}, {278941742, 1473465000000}, {285253072, 1476835000000},
		{294851037, 1479600000000}, {297621225, 1481846000000}, {328594461, 1482969000000},
		{337808429, 1487707000000}, {341546272, 1487782000000}, {352940995, 1487894000000},
		{369669043, 1490918000000}, {400169472, 1501459000000}, {805158066, 1563208000000},
		{1974255900, 1634000000000}, {5520018289, 1721847912670},
	}
	// The dataset is noisy around 2015 (IDs are not strictly monotonic);
	// sort by ID so interpolation is well defined.
	sort.Slice(a, func(i, j int) bool { return a[i].ID < a[j].ID })
	return a
}()

// tgEstimateCreation returns the estimated creation time for a Telegram user
// ID and whether the ID falls beyond the last anchor (estimate is weaker).
func tgEstimateCreation(id int64) (time.Time, bool) {
	first, last := tgAnchors[0], tgAnchors[len(tgAnchors)-1]
	if id <= first.ID {
		return time.UnixMilli(first.MS), false
	}
	if id >= last.ID {
		return time.UnixMilli(last.MS), true
	}
	i := sort.Search(len(tgAnchors), func(i int) bool { return tgAnchors[i].ID >= id })
	lo, hi := tgAnchors[i-1], tgAnchors[i]
	frac := float64(id-lo.ID) / float64(hi.ID-lo.ID)
	ms := lo.MS + int64(frac*float64(hi.MS-lo.MS))
	return time.UnixMilli(ms), false
}

// checkTelegramBot verifies ownership and age in one step: the user sends
// their account code as a message to our bot, and the Bot API hands us their
// authenticated numeric ID and username. Pending updates are retained for 24
// hours, and we never advance the offset, so the message stays visible to
// checks and re-checks.
func (a *App) checkTelegramBot(handle, claimCode string) string {
	url := "https://api.telegram.org/bot" + a.cfg.TgBotToken + "/getUpdates?limit=100"
	resp, err := verifClient.Get(url)
	if err != nil {
		return "telegram API unreachable; re-check later"
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool `json:"ok"`
		Result []struct {
			Message struct {
				Text string `json:"text"`
				From struct {
					ID       int64  `json:"id"`
					Username string `json:"username"`
				} `json:"from"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil || !out.OK {
		return "telegram API response unreadable; re-check later"
	}
	for _, u := range out.Result {
		if !strings.EqualFold(u.Message.From.Username, handle) || !strings.Contains(u.Message.Text, claimCode) {
			continue
		}
		created, weak := tgEstimateCreation(u.Message.From.ID)
		age := time.Since(created)
		years := age.Hours() / 24 / 365
		verdict := "age OK"
		if age < time.Duration(verifMinAgeYears)*365*24*time.Hour {
			verdict = fmt.Sprintf("UNDER %d YEARS", verifMinAgeYears)
		}
		note := fmt.Sprintf("message verified from @%s (id %d); account created ~%s, about %.1f years old (%s)",
			u.Message.From.Username, u.Message.From.ID, created.Format("Jan 2006"), years, verdict)
		if weak {
			note += "; id beyond the newest anchor, estimate is a lower bound on recency"
		}
		return note
	}
	return "no matching message found: send your account code as a Telegram message to the bot from @" + handle + ", then re-check (messages are visible to the check for 24 hours)"
}
