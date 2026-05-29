package edit

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// ReloadStatus is the wire shape of the framework's /reload/status endpoint.
// Only the fields abctl uses are decoded.
type ReloadStatus struct {
	LastSuccessUnix int64  `json:"last_success_unix"`
	ReloadsOK       uint64 `json:"reloads_ok"`
	ReloadsFailed   uint64 `json:"reloads_failed"`
	LastError       string `json:"last_error"`
}

// PollResultStatus is a sum type for PollUntilReloaded outcomes.
type PollResultStatus int

const (
	PollUnknown PollResultStatus = iota
	PollSuccess
	PollFailure
	PollTimeout
)

// PollResult is what PollUntilReloaded returns.
type PollResult struct {
	Status    PollResultStatus
	LastError string // populated when Status == PollFailure
}

// pollInterval is the cadence between /reload/status fetches. 1s balances
// user-visible spinner progress with not hammering the cluster on slow
// reloads.
const pollInterval = 1 * time.Second

// PollUntilReloaded watches statusURL/reload/status until either:
//   - LastSuccessUnix > applyTime.Unix() → PollSuccess.
//   - ReloadsFailed exceeds the value at first successful poll → PollFailure
//     with LastError populated.
//   - ctx is done → PollTimeout. (Caller is expected to set a 120s timeout
//     via context.WithTimeout.)
//
// HTTP errors (network, non-200) are retried until ctx expires; we treat
// them as "framework not yet reachable, keep waiting."
func PollUntilReloaded(ctx context.Context, statusURL string, applyTime time.Time) PollResult {
	url := statusURL + "/reload/status"
	client := &http.Client{Timeout: 2 * time.Second}

	var baselineFailed uint64
	first := true

	for {
		select {
		case <-ctx.Done():
			return PollResult{Status: PollTimeout}
		default:
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return PollResult{Status: PollTimeout}
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == 200 {
			var rs ReloadStatus
			decodeErr := json.NewDecoder(resp.Body).Decode(&rs)
			resp.Body.Close()
			if decodeErr == nil {
				if first {
					baselineFailed = rs.ReloadsFailed
					first = false
				}
				if rs.LastSuccessUnix > applyTime.Unix() {
					return PollResult{Status: PollSuccess}
				}
				if rs.ReloadsFailed > baselineFailed {
					return PollResult{Status: PollFailure, LastError: rs.LastError}
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}

		select {
		case <-ctx.Done():
			return PollResult{Status: PollTimeout}
		case <-time.After(pollInterval):
		}
	}
}
